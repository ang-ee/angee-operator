\set ON_ERROR_STOP on
CREATE ROLE appowner LOGIN PASSWORD 'appownerpw';
CREATE ROLE reader;
CREATE DATABASE app OWNER appowner;
\connect app
CREATE EXTENSION vector;
SET ROLE appowner;
CREATE TABLE items (
  id        serial PRIMARY KEY,
  name      text NOT NULL,
  embedding vector(3) NOT NULL
);
INSERT INTO items (name, embedding) VALUES
  ('alpha', '[1,2,3]'),
  ('beta',  '[0.5,-1.25,3.75]'),
  ('gamma', '[1e-3,2.5e2,-7]'),
  ('delta', '[0,0,0]');
CREATE INDEX items_embedding_hnsw ON items USING hnsw (embedding vector_l2_ops);
GRANT SELECT ON items TO reader;
RESET ROLE;
SELECT current_database(), (SELECT count(*) FROM items) AS rows,
       (SELECT tableowner FROM pg_tables WHERE tablename = 'items') AS owner,
       (SELECT relacl::text FROM pg_class WHERE relname = 'items') AS acl;
