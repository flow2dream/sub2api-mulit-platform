package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestKiroUsageDTOExposesEstimationButKeepsCreditsAdminOnly(t *testing.T) {
	credits := 0.25
	log := &service.UsageLog{UsageSource: "mixed", UpstreamUsage: &kiro.Usage{Source: "mixed", Credits: &credits}}
	user, _ := json.Marshal(UsageLogFromService(log))
	admin, _ := json.Marshal(UsageLogFromServiceAdmin(log))
	var userFields, adminFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(user, &userFields))
	require.NoError(t, json.Unmarshal(admin, &adminFields))
	require.JSONEq(t, `"mixed"`, string(userFields["usage_source"]))
	require.NotContains(t, userFields, "upstream_usage")
	require.Contains(t, string(adminFields["upstream_usage"]), `"credits":0.25`)
}
