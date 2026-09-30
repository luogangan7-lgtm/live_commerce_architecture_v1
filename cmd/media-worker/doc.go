// Command media-worker owns the process that runs the live-media River queue: LiveKit Cloud Egress
// start/stop, recovery and browser-input plan execution through internal/live.NewMediaClient, on the
// media worker and executor DB logins. supervisor.go optionally runs it under a restart supervisor
// (COMMERCE_MEDIA_RECOVERY_SUPERVISED=1) so a crashed child is recovered from leased, fenced jobs.
//
// It never serves HTTP, never decides who may go live, never reads provider credentials outside
// internal/integrations/livekit's sealed keyring, and stays off unless the LiveKit worker
// environment enables it. External host: the LiveKit Cloud project URLs named in that environment
// (Egress API).
package main
