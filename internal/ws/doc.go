// Package ws upgrades HTTP connections, runs one read loop and one write pump per connection, and
// routes each action in protocol/ to the sheet, lock or replica registry. Arrives with S2.
package ws
