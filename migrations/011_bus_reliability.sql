-- Additive upgrade: preserve all existing presence and durable messages.
ALTER TABLE bus_presence ADD COLUMN idle_since timestamptz;
