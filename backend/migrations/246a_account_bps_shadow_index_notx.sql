-- 246a_account_bps_shadow_index_notx.sql
-- 一母一 bps 影（与 154a 的 spark 部分唯一索引并列；两维度可各存一影）。
-- CREATE INDEX CONCURRENTLY 不能在事务内执行，故独立 notx 迁移。
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_accounts_bps_shadow_per_parent
    ON accounts (parent_account_id)
    WHERE parent_account_id IS NOT NULL AND quota_dimension = 'bps' AND deleted_at IS NULL;
