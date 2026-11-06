\set ON_ERROR_STOP on
CREATE TABLE t (
  id  int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  v   text NOT NULL,
  j   jsonb,
  ts  timestamptz DEFAULT now()
);
INSERT INTO t (v, j) SELECT 'row ' || g, jsonb_build_object('g', g) FROM generate_series(1, 1000) g;
CREATE VIEW t_even AS SELECT * FROM t WHERE id % 2 = 0;
CREATE FUNCTION t_count() RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM t';
SELECT version(), t_count();
