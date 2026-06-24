-- fn is a random 24-char id, unique by generation, but was never indexed:
-- lookups (lookupFile) and the signed-upload upsert both filtered on fn with a
-- full table scan. A unique index makes them point operations and lets the
-- upload path use INSERT ... ON CONFLICT (fn) to dedupe a replayed PUT.
create unique index files_fn_key on files (fn);
