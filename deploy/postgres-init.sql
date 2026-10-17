-- The Postgres container creates the "homecloud" database (vmd) itself; agentd keeps its own database
-- so the two services never share tables and can be moved to separate servers later.
CREATE DATABASE agentd;
