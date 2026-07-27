-- Stealers persists each successful visitor UID for a harvest round. A mature
-- plot can exceed the old 256-byte envelope before its 40% steal quota is used.
ALTER TABLE farm_plot
  MODIFY COLUMN `blob` VARBINARY(512) NOT NULL;
