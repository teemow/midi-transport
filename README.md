# midi-transport

MIDI/OSC transport backends extracted from
[mcp-midi-controller](https://github.com/teemow/mcp-midi-controller). This is the
IO layer: the pluggable `Transport` interface and its implementations (OSC over
UDP, USB MIDI via ALSA, vendor USB HID, BLE-MIDI over BlueZ/D-Bus, and the AUv3
bridge), plus the off-MCP control hub and the LAN probe-dump receiver.

It pairs with [midi-device](https://github.com/teemow/midi-device) (the
declarative device kernel). Transports move bytes; bring your own control loop —
orchestration (desired/observed state, feedback, scene recall) stays in the
consumer.

## Install

```
go get github.com/teemow/midi-transport
```

## Packages

- `transport` (module root) — the `Transport` interface plus the shared value
  types (`Event`, `EventKind`, `Endpoint`). Implement `Transport` to add a new
  protocol.
- `transport/osc` — OSC over UDP (e.g. a Behringer X32).
- `transport/usbmidi` — USB MIDI ports via ALSA (gomidi). Cgo-gated, with a
  `!cgo` stub.
- `transport/usbhid` — vendor USB HID pipes (raw reports). Linux-gated, with a
  `!linux` stub.
- `transport/blemidi` — BLE-MIDI discovery/pairing/data path over BlueZ/D-Bus.
  Cgo-gated ALSA helper, with a `!cgo` stub.
- `transport/auv3midi` — bridges the AUv3 brain plugin over the `midicontrol`
  WebSocket hub.
- `transport/midicontrol` — the off-MCP WebSocket hub the AUv3 brain dials in to.
- `transport/auv3` — the LAN listener that ingests AUv3 parameter-tree dumps
  POSTed by the [auv3-probe](https://github.com/teemow/auv3-probe) iPad app.

Platform note: the BLE-MIDI and USB-HID/USB-MIDI backends are Linux/cgo
specific and ship build-tagged stubs, so cross-platform and kernel-only
consumers still compile.

## License

See [LICENSE](LICENSE).
