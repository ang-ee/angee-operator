\set ON_ERROR_STOP on
DROP DATABASE IF EXISTS bigdb;
CREATE DATABASE bigdb;
\connect bigdb
CREATE EXTENSION vector;
CREATE TABLE big AS
  SELECT g AS id,
         md5(g::text) AS a,
         md5((g * 7)::text) AS b,
         timestamptz '2026-01-01' + g * interval '1 second' AS ts,
         ARRAY[(g % 97)::real, (g % 89)::real / 10, -(g % 83)::real]::vector(3) AS embedding
  FROM generate_series(1, 2000000) g;
ALTER TABLE big ADD PRIMARY KEY (id);
CREATE TABLE small (id int PRIMARY KEY, note text);
INSERT INTO small SELECT g, 'note ' || g FROM generate_series(1, 1000) g;
SELECT pg_size_pretty(pg_total_relation_size('big')) AS big_total,
       pg_size_pretty(pg_relation_size('big')) AS big_heap,
       pg_size_pretty(pg_database_size('bigdb')) AS db_size;
