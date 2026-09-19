-- Runs once, when the database is first made. The passwords come from the
-- container's environment, never from this file or a command line.
\getenv owner_password NOTES_OWNER_PASSWORD
\getenv app_password NOTES_APP_PASSWORD

SELECT current_user = 'notes_admin' AS admin_ok \gset
\if :admin_ok
\else
  \warn 'the superuser must be notes_admin'
  \quit 1
\endif

SELECT format('CREATE ROLE notes_owner LOGIN PASSWORD %L', :'owner_password') \gexec
SELECT format('CREATE ROLE notes_app LOGIN PASSWORD %L', :'app_password') \gexec
CREATE EXTENSION IF NOT EXISTS vector;
ALTER DATABASE notes OWNER TO notes_owner;
ALTER SCHEMA public OWNER TO notes_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO notes_app;
GRANT CONNECT ON DATABASE notes TO notes_app;
