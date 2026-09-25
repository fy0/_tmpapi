package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyKlnoConvergenceMigration(t *testing.T) {
	content, err := FS.ReadFile("240z_converge_legacy_klno_schema.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	// 四列 legacy 独占观测列全部以 IF EXISTS 幂等删除。
	for _, col := range []string{"turn_state", "turn_state_overridden", "turn_state_source", "turn_state_sent"} {
		require.Contains(t, sql, "DROP COLUMN IF EXISTS "+col)
	}
	// request_type CHECK 的恢复必须是有条件的：存在 probe 行时不得收紧约束。
	require.Contains(t, sql, "CHECK (request_type >= 0 AND request_type <= 5)")
	require.Less(t, strings.Index(sql, "NOT EXISTS (SELECT 1 FROM usage_logs WHERE request_type = 6)"), strings.Index(sql, "ADD CONSTRAINT usage_logs_request_type_check"))
	// legacy 独占迁移记账行必须清理。
	require.Contains(t, sql, "DELETE FROM schema_migrations")
	for _, name := range []string{
		"239_add_usage_log_turn_state.sql",
		"240_add_usage_log_turn_state_source.sql",
		"241_add_usage_log_turn_state_sent.sql",
		"242_document_turn_state_seed_source.sql",
		"243_drop_turn_state_seed_source.sql",
		"244_allow_probe_usage_request_type.sql",
		"245_clear_account_level_turn_state_hold.sql",
	} {
		require.Contains(t, sql, name)
	}
}
