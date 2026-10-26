-- Native wake is opt-in per message and supported only by reporting recipients.
ALTER TABLE bus_presence ADD COLUMN wake_supported boolean NOT NULL DEFAULT false;
ALTER TABLE bus_messages ADD COLUMN wake_requested boolean NOT NULL DEFAULT false;
