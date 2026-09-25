package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyConvergeRepairMigration(t *testing.T) {
	content, err := FS.ReadFile("245z_repair_after_db_converge.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	// 收敛镜像的孤儿记账行必须清掉。
	require.Contains(t, sql, "DELETE FROM schema_migrations")
	require.Contains(t, sql, "240z_converge_legacy_klno_schema.sql")
	// atlas 基线只修正收敛镜像写坏的那一行，不误伤其他状态。
	require.Contains(t, sql, "WHERE version = '240_affiliate_ledger_operation_id'")
	require.Contains(t, sql, "'245_clear_account_level_turn_state_hold'")
}
