-- Multi-agent scoping (docs/adr/0006_multi_agent_scoping.md): the gateway
-- now resolves a policy per request by agent_id instead of loading a single
-- policy at boot. That lookup must be deterministic, so every policy row
-- now belongs to exactly one agent, one-to-one.
--
-- Pre-existing rows with a NULL agent_id must be assigned one before this
-- migration runs, or it will fail on the NOT NULL step.
ALTER TABLE policies ALTER COLUMN agent_id SET NOT NULL;
ALTER TABLE policies ADD CONSTRAINT policies_agent_id_key UNIQUE (agent_id);
