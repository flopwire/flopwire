-- Status metadata outlives message retention. No body, refs, repo or title.
CREATE TABLE bus_delivery_failures (
  message_id text PRIMARY KEY,
  from_user uuid NOT NULL REFERENCES users(id),
  from_device uuid REFERENCES devices(id),
  from_session text NOT NULL,
  from_agent text NOT NULL,
  state text NOT NULL,
  reason text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  lease_device uuid REFERENCES devices(id),
  lease_token text,
  lease_until timestamptz,
  acked_at timestamptz
);
CREATE INDEX bus_delivery_failures_sender ON bus_delivery_failures(from_user, from_device, created_at) WHERE acked_at IS NULL;
CREATE FUNCTION bus_record_delivery_failure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.state IN ('queued','held','claimed') AND NEW.state IN ('expired','undelivered') THEN
    INSERT INTO bus_delivery_failures(message_id,from_user,from_device,from_session,from_agent,state,reason)
      VALUES(NEW.id,NEW.from_user,NEW.from_device,NEW.from_session,NEW.from_agent,NEW.state,
        CASE WHEN NEW.state='expired' THEN 'expired' ELSE NEW.reason END)
      ON CONFLICT DO NOTHING;
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER bus_delivery_failure AFTER UPDATE OF state ON bus_messages
  FOR EACH ROW EXECUTE FUNCTION bus_record_delivery_failure();
-- Existing durable failures are retained on upgrade too.
INSERT INTO bus_delivery_failures(message_id,from_user,from_device,from_session,from_agent,state,reason)
 SELECT id,from_user,from_device,from_session,from_agent,state,
   CASE WHEN state='expired' THEN 'expired' ELSE reason END
 FROM bus_messages WHERE state IN ('expired','undelivered') ON CONFLICT DO NOTHING;
