-- Creates the copilot's least-privilege roles. Runs once, as the superuser, from the postgres
-- image's /docker-entrypoint-initdb.d/. Passwords come from container environment variables
-- (read with \getenv, psql 15+); no password literal appears in this file.
--   COPILOT_APP_PASSWORD, COPILOT_RO_PASSWORD, FAULT_INJECTOR_PASSWORD
-- Table grants are applied later by copilot/grants.sql (apply_schema), once tables exist.
\set ON_ERROR_STOP on

\getenv app_pw COPILOT_APP_PASSWORD
\getenv ro_pw COPILOT_RO_PASSWORD
\getenv fi_pw FAULT_INJECTOR_PASSWORD

-- Existence-guarded so re-running is safe. (psql variables are not expanded inside a DO body,
-- so the guard is a WHERE NOT EXISTS and \gexec runs the generated statement.)
SELECT format('CREATE ROLE copilot_app LOGIN PASSWORD %L', :'app_pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_app') \gexec
SELECT format('CREATE ROLE copilot_ro LOGIN PASSWORD %L', :'ro_pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_ro') \gexec
SELECT format('CREATE ROLE fault_injector LOGIN PASSWORD %L', :'fi_pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fault_injector') \gexec

-- Let the roles use the schema (CREATE on public stays with the owner only).
GRANT USAGE ON SCHEMA public TO copilot_app, copilot_ro, fault_injector;
