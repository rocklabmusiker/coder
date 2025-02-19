package insights_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/v2/agent/agenttest"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/prometheusmetrics/insights"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/provisioner/echo"
	"github.com/coder/coder/v2/testutil"
)

func TestCollect_TemplateInsights(t *testing.T) {
	t.Parallel()

	logger := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})
	db, ps := dbtestutil.NewDB(t)

	options := &coderdtest.Options{
		IncludeProvisionerDaemon:  true,
		AgentStatsRefreshInterval: time.Millisecond * 100,
		Database:                  db,
		Pubsub:                    ps,
	}
	client := coderdtest.New(t, options)

	// Given
	// Initialize metrics collector
	mc, err := insights.NewMetricsCollector(db, logger, 0, time.Second)
	require.NoError(t, err)

	registry := prometheus.NewRegistry()
	registry.Register(mc)

	// Create two users, one that will appear in the report and another that
	// won't (due to not having/using a workspace).
	user := coderdtest.CreateFirstUser(t, client)
	_, _ = coderdtest.CreateAnotherUser(t, client, user.OrganizationID)
	authToken := uuid.NewString()
	version := coderdtest.CreateTemplateVersion(t, client, user.OrganizationID, &echo.Responses{
		Parse:          echo.ParseComplete,
		ProvisionPlan:  echo.PlanComplete,
		ProvisionApply: echo.ProvisionApplyWithAgent(authToken),
	})
	template := coderdtest.CreateTemplate(t, client, user.OrganizationID, version.ID)
	require.Empty(t, template.BuildTimeStats[codersdk.WorkspaceTransitionStart])

	coderdtest.AwaitTemplateVersionJobCompleted(t, client, version.ID)
	workspace := coderdtest.CreateWorkspace(t, client, user.OrganizationID, template.ID)
	coderdtest.AwaitWorkspaceBuildJobCompleted(t, client, workspace.LatestBuild.ID)

	// Start an agent so that we can generate stats.
	_ = agenttest.New(t, client.URL, authToken)
	resources := coderdtest.AwaitWorkspaceAgents(t, client, workspace.ID)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitLong)
	defer cancel()

	// Run metrics collector
	closeFunc, err := mc.Run(ctx)
	require.NoError(t, err)
	defer closeFunc()

	// Connect to the agent to generate usage/latency stats.
	conn, err := client.DialWorkspaceAgent(ctx, resources[0].Agents[0].ID, &codersdk.DialWorkspaceAgentOptions{
		Logger: logger.Named("client"),
	})
	require.NoError(t, err)
	defer conn.Close()

	sshConn, err := conn.SSHClient(ctx)
	require.NoError(t, err)
	defer sshConn.Close()

	sess, err := sshConn.NewSession()
	require.NoError(t, err)
	defer sess.Close()

	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	sess.Stdin = r
	sess.Stdout = io.Discard
	err = sess.Start("cat")
	require.NoError(t, err)

	goldenFile, err := os.ReadFile("testdata/insights-metrics.json")
	require.NoError(t, err)
	golden := map[string]int{}
	err = json.Unmarshal(goldenFile, &golden)
	require.NoError(t, err)

	collected := map[string]int{}
	assert.Eventuallyf(t, func() bool {
		// When
		metrics, err := registry.Gather()
		require.NoError(t, err)

		// Then
		for _, metric := range metrics {
			switch metric.GetName() {
			case "coderd_insights_templates_active_users":
				for _, m := range metric.Metric {
					collected[metric.GetName()] = int(m.Gauge.GetValue())
				}
			default:
				require.FailNowf(t, "unexpected metric collected", "metric: %s", metric.GetName())
			}
		}

		return assert.ObjectsAreEqualValues(golden, collected)
	}, testutil.WaitMedium, testutil.IntervalFast, "template insights are missing")

	// We got our latency metrics, close the connection.
	_ = sess.Close()
	_ = sshConn.Close()

	require.EqualValues(t, golden, collected)
}
