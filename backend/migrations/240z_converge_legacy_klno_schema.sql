-- 收敛 klno-legacy（重建前 klno 快照 klno-legacy-2026-09-23）数据库到新 klno 的
-- schema 形状。一次性迁移：让 legacy 形状的数据库与全新 klno 部署完全一致。
--
-- legacy 独占的 239-245 迁移留下三类残留：
--   1. usage_logs.turn_state / turn_state_overridden / turn_state_source /
--      turn_state_sent 四个观测列（klno 无对应代码路径）；
--   2. usage_logs_request_type_check 被 244 放宽到 <=6（probe 记账类型，klno
--      期望的上游形状是 <=5）；
--   3. schema_migrations 里七条 legacy 独占文件名的记账行。
--
-- 全部操作幂等：全新 klno 库上每一句都是 no-op，可以安全留在迁移序列里。
--
-- 注意单向性：列被 DROP 后旧 legacy 镜像无法回滚（其 usage_logs 写入语句引用
-- 这些列）。需要回滚窗口的话先部普通 klno 镜像验证（残留列无害），再切本镜像。

SET LOCAL lock_timeout = '5s';

-- 1. 删除 legacy 独占的 turn_state 观测列。全部可空、无索引、无约束，DROP 即可。
ALTER TABLE usage_logs DROP COLUMN IF EXISTS turn_state;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS turn_state_overridden;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS turn_state_source;
ALTER TABLE usage_logs DROP COLUMN IF EXISTS turn_state_sent;

-- 2. request_type CHECK 恢复 klno 的 <=5 形状（188_allow_live_usage_request_type
--    的定义）。仅当库中没有 probe(request_type=6）行时恢复：有数据时保留放宽
--    约束，宁可形状略宽也不丢用量记录。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM usage_logs WHERE request_type = 6) THEN
        ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_request_type_check;
        ALTER TABLE usage_logs
            ADD CONSTRAINT usage_logs_request_type_check
            CHECK (request_type >= 0 AND request_type <= 5);
    ELSE
        RAISE WARNING 'usage_logs contains request_type=6 (legacy probe) rows; keeping widened request_type_check (<=6)';
    END IF;
END $$;

-- 3. 清掉 legacy 独占迁移的记账行。runner 只校验 FS 内文件的 checksum，这些行
--    只是噪音；删掉后与全新 klno 库的 schema_migrations 一致。
DELETE FROM schema_migrations WHERE filename IN (
    '239_add_usage_log_turn_state.sql',
    '240_add_usage_log_turn_state_source.sql',
    '241_add_usage_log_turn_state_sent.sql',
    '242_document_turn_state_seed_source.sql',
    '243_drop_turn_state_seed_source.sql',
    '244_allow_probe_usage_request_type.sql',
    '245_clear_account_level_turn_state_hold.sql'
);

-- 4. atlas_schema_revisions 基线行对齐到 klno 的尾文件基线。该表只被 Atlas
--    外部工具读取、应用层不用；有行才更新，无行则留给 runner 的基线逻辑处理。
UPDATE atlas_schema_revisions
SET version = '240_affiliate_ledger_operation_id',
    description = '240_affiliate_ledger_operation_id',
    hash = '3823bfea5f64feb58f5fcebc341c6877eaefc97834b4cd652c8e83ad08ed78da'
WHERE version <> '240_affiliate_ledger_operation_id';
