-- Migration: 000001_init (down) — drops everything 000001_init.up.sql created, dependents first.
DROP TABLE IF EXISTS cluster_state;
DROP TABLE IF EXISTS cache_entries;
DROP TABLE IF EXISTS sheets;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS projects;
