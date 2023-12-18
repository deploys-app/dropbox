create table files (
	fn         text     not null,
	project_id integer  not null,
	size       interger not null,
	filename   text     not null,
	ttl        integer  not null,
	created_at integer  not null default current_timestamp
);
create index files_project_id_created_at_idx on files (project_id, created_at);
