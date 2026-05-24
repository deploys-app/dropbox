create table files (
	fn         text        not null,
	project_id text        not null,
	size       bigint      not null,
	filename   text        not null,
	ttl        integer     not null,
	created_at timestamptz not null default now(),
	expires_at timestamptz,
	token      text        not null default ''
);
create index files_project_id_created_at_idx on files (project_id, created_at);
create index files_expires_at_idx on files (expires_at);
