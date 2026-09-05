ALTER TABLE policies DROP CONSTRAINT policies_agent_id_key;
ALTER TABLE policies ALTER COLUMN agent_id DROP NOT NULL;
