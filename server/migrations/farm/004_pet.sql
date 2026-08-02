-- 004_pet.sql
-- 看家狗状态以 JSON 写入 player.pet_blob；64 字节无法容纳拥有、等级和狗盆到期时间。

ALTER TABLE player
  MODIFY COLUMN pet_blob VARBINARY(256) NOT NULL;
