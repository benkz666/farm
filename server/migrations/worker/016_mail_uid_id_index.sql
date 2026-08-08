-- MailList 按 uid 过滤并按 id 倒序；该联合索引避免大邮箱额外排序。
CREATE INDEX idx_mail_uid_id_desc ON mail (uid, id DESC);
