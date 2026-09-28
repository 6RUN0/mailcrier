// Package spool keeps messages on disk until every target has them.
//
// A message is written ahead of its delivery: the message and a sidecar
// with the state of each target go to tmp/, are synced and moved into
// queue/, and the sidecar is rewritten after each attempt. A process
// killed at any point loses at most the record of a delivery that
// happened, which a later run repeats: delivery is at least once.
package spool
