-- 002_items.sql
-- 期 2：背包/仓库共用 item 表（架构 5.8 / 期 2 计划 Task 4）。
-- kind：1 种子 2 化肥 3 狗粮 4 果实

CREATE TABLE IF NOT EXISTS item (
  uid     BIGINT UNSIGNED   NOT NULL,
  kind    TINYINT UNSIGNED  NOT NULL,
  item_id SMALLINT UNSIGNED NOT NULL,
  count   INT UNSIGNED      NOT NULL,
  PRIMARY KEY (uid, kind, item_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
