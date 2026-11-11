-- Runs once, when the postgres volume is first initialised (docker-entrypoint-initdb.d).
-- redlite_test is the database the persistence tests use (REDLITE_TEST_DATABASE_URL); each test
-- makes and drops its own schema in it, so it never touches the cache's own database, redlite.
-- A volume created before this file existed: docker compose exec postgres createdb -U postgres redlite_test
CREATE DATABASE redlite_test;
