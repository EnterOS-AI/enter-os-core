-- Down migration for memory_v2 plugin schema (RFC #2728).
DROP TABLE IF EXISTS memory_records;
DROP TABLE IF EXISTS memory_namespaces;
