alter table files add column expires_at timestamptz;
create index files_expires_at_idx on files (expires_at);
