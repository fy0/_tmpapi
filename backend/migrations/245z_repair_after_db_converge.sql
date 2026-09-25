-- 修复 fix/legacy-db-converge 镜像（那次收敛按错误假设把 DB 对齐到不含 KlN 定制的
-- schema）留下的记账残留。
--
-- 背景：该镜像的 240z_converge_legacy_klno_schema.sql 删掉了 legacy 独占迁移
-- （239-245）在 schema_migrations 里的行并 DROP 了 turn_state 观测列。klno 换回
-- KlN-4096 基线后，被删行对应的迁移文件回到 FS 里且全部幂等，启动时会自动重放、
-- 重建列与约束——数据结构由那边自愈，本文件只负责把两张记账表收拾干净：
--   1. schema_migrations 里的 '240z_converge_legacy_klno_schema.sql' 孤儿行
--      （对应文件不在本分支 FS，runner 不校验，但留着是噪音）；
--   2. atlas_schema_revisions 基线行——收敛镜像把它改成了 '240_affiliate_...'，
--      本分支的尾文件基线是 '245_clear_account_level_turn_state_hold'。
--
-- 全幂等：没跑过收敛镜像的库上每一句都是 no-op。

DELETE FROM schema_migrations
WHERE filename = '240z_converge_legacy_klno_schema.sql';

UPDATE atlas_schema_revisions
SET version = '245_clear_account_level_turn_state_hold',
    description = '245_clear_account_level_turn_state_hold',
    hash = '4c971ec0938b04777ada9468f8e979c5ac4567165d73757366427e11ab18dfe4'
WHERE version = '240_affiliate_ledger_operation_id';
