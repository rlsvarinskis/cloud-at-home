-- Unexpected exits are counted separately from failed attempts to reach the node, so a VM that
-- boots and then crashes repeatedly backs off even though every call to its node succeeds.
ALTER TABLE vms ADD COLUMN crashes integer NOT NULL DEFAULT 0;
