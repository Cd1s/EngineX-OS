# Engine Remote Library (EaaS) protocol stubs

`enginelibrary/` and `networktrust/` are the protobuf messages and gRPC service
stubs of Engine's Remote Library ("Engine as a Service"), copied unmodified from
[github.com/icedream/go-stagelinq](https://github.com/icedream/go-stagelinq)
commit `b603e9fdb7be1ba18d09abecf6e2742702dac997` (`eaas/proto/`), generated there
by `protoc-gen-go` v1.31.0 and `protoc-gen-go-grpc` v1.5.1. `LICENSE` is that
project's MIT licence, copied verbatim.

Checked against the descriptors embedded in Engine 5.3.0 (`/usr/Engine/Engine`,
sha256 `51dd0cc1…`): every message, field number, type, oneof, map and enum value
matches. The only differences are `optional` markers (field presence, not the wire
format): upstream marks most scalar fields optional, Engine's `Loop.loop_in` and
`loop_out` are optional and the rest are not. Upstream also spells Engine's
`map<uint32, QuickCue>` and `map<uint32, Loop>` as the repeated `…Entry {key = 1;
value = 2}` messages that protobuf defines a map to be on the wire.

`pkg/webui/rekordbox_eaas.go` serves these to the deck's own Engine.
