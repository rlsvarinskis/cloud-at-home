-- The operator's drain decision is stored apart from observed health (state), so an outage cannot erase it.
ALTER TABLE nodes ADD COLUMN drain boolean NOT NULL DEFAULT false;
UPDATE nodes SET drain = true, state = 'ready' WHERE state = 'draining';
