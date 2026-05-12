create table files (
	fn         text        not null,
	project_id text        not null,
	size       bigint      not null,
	filename   text        not null,
	ttl        integer     not null,
	created_at timestamptz not null default now()
);
create index files_project_id_created_at_idx on files (project_id, created_at);
