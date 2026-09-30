// Package e2e holds the two-device end-to-end sync test (spec §11 B4). It
// drives a real server (Compose: Postgres, MinIO, flopwire) and two
// `flopwire agent run` processes with separate homes, configs and indexes.
// It runs only with FLOPWIRE_E2E=1 and the environment scripts/e2e-sync.sh
// sets up; see docs/two-laptop.md for the real two-laptop setup.
package e2e
