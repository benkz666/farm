-- 跨农场预占（互助额度、偷菜冻结金币）此前只存在于 Gateway 进程内存里，进程消失
-- 就再没有任何人负责回滚，冻结的金币会永久失踪。改为随访客聚合一起持久化。
--
-- 用 information_schema 判断列是否已存在，因为 scripts/run.sh 每次启动都会重跑全部
-- 迁移，而 ADD COLUMN 本身不幂等（列已存在时直接报错）。
--
-- 4096 字节的依据：farm.maxCrossPending 把在途预占限制为 16 条，单条 JSON 约 210
-- 字节（含 map 键），上限约 3.3 KB。默认空串让注册路径的 INSERT 无需改动。
SET @missing := (
  SELECT COUNT(*) = 0 FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'player'
    AND COLUMN_NAME = 'cross_blob'
);
SET @stmt := IF(
  @missing,
  'ALTER TABLE player ADD COLUMN cross_blob VARBINARY(4096) NOT NULL DEFAULT ''''',
  'DO 0'
);
PREPARE add_cross_blob FROM @stmt;
EXECUTE add_cross_blob;
DEALLOCATE PREPARE add_cross_blob;
