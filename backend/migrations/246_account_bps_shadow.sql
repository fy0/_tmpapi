-- 246_account_bps_shadow.sql
-- 新增 quota_dimension='bps' 影子维度：Basispoints 渠道副本。
-- 与 spark 影子同构（parent_account_id 透传凭据、一母一影），但消耗普通
-- ChatGPT plan 配额而非 bengalfox 窗口，故独立成维度。

-- 放宽维度 CHECK：存量行仍满足 ('global','spark') ⊂ ('global','spark','bps')，
-- drop+NOT VALID+VALIDATE 是修改枚举 CHECK 的标准幂等做法。
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS chk_accounts_quota_dimension;
ALTER TABLE accounts ADD CONSTRAINT chk_accounts_quota_dimension
    CHECK (quota_dimension IN ('global','spark','bps')) NOT VALID;
ALTER TABLE accounts VALIDATE CONSTRAINT chk_accounts_quota_dimension;
