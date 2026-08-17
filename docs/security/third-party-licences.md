# Third-party licences

Required by §84.19: every piece of external source the project depends on has
its licence identified and recorded.

This is a register of **direct** dependencies — the ones this repository names
in `go.mod` and `pubspec.yaml`. Transitive dependencies are resolved by the
package managers and are covered by the generated reports described at the
bottom, which are the authority for a release; this file is the authority for
what was chosen and why.

The one entry that constrains distribution is
[`libsignal_protocol_dart`](#end-to-end-encryption) — read that section before
publishing a binary.

---

## End-to-end encryption

| Package | Version | Licence | Where |
|---|---|---|---|
| `libsignal_protocol_dart` | ^0.8.2 | **GPL-3.0** | `apps/mobile` |

X3DH and the Double Ratchet, as a Dart port of Signal's own library. §84 rules
16 and 17 forbid inventing a construction or assembling one from primitives, so
a reviewed implementation is not a convenience here — it is the requirement.
The app supplies transport, storage and identity around it and performs no
cryptography of its own.

**This is the only copyleft dependency in the project, and it is a strong one.**
GPL-3.0 is not the LGPL: linking it into the Flutter app means the app as
distributed is a derived work, and the licence obliges whoever distributes it to
offer the corresponding source of the whole under GPL-3.0-compatible terms. That
is a decision about how SOBH is published, not a technical detail, and it must
be settled before a binary reaches a store. The alternatives are to obtain a
separate licence from the copyright holders, or to replace the dependency with a
differently-licensed reviewed implementation. Writing one instead is not an
option §84 leaves open.

Nothing GPL-licensed is linked into the backend.

---

## Backend (Go)

Every entry is a permissive licence — BSD-3-Clause, MIT or Apache-2.0 — which
imposes attribution but no obligation on how SOBH itself is licensed.

| Module | Version | Licence |
|---|---|---|
| `github.com/go-chi/chi/v5` | v5.3.1 | MIT |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | MIT |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/gorilla/websocket` | v1.5.3 | BSD-2-Clause |
| `github.com/jackc/pgx/v5` | v5.7.6 | MIT |
| `github.com/minio/minio-go/v7` | v7.0.96 | Apache-2.0 |
| `github.com/nats-io/nats.go` | v1.48.0 | Apache-2.0 |
| `github.com/prometheus/client_golang` | v1.20.5 | Apache-2.0 |
| `github.com/redis/go-redis/v9` | v9.15.0 | BSD-2-Clause |
| `golang.org/x/crypto` | v0.45.0 | BSD-3-Clause |
| `golang.org/x/image` | v0.32.0 | BSD-3-Clause |
| `golang.org/x/net` | v0.47.0 | BSD-3-Clause |

### Test-only

These are compiled into tests and never into a shipped binary. They are listed
because `go.mod` does not distinguish them, so a reader would otherwise assume
they ship.

| Module | Version | Licence | Used for |
|---|---|---|---|
| `github.com/alicebob/miniredis/v2` | v2.35.0 | MIT | A real Redis protocol implementation in-process |
| `github.com/nats-io/nats-server/v2` | v2.10.27 | Apache-2.0 | An in-process NATS with JetStream for the end-to-end suite |

---

## Mobile app (Dart / Flutter)

All BSD-3-Clause, MIT or Apache-2.0 except the encryption entry recorded above.

| Package | Version | Licence |
|---|---|---|
| `flutter`, `flutter_localizations` | SDK | BSD-3-Clause |
| `flutter_riverpod`, `riverpod_annotation` | ^2.6.1 | MIT |
| `go_router` | ^14.8.1 | BSD-3-Clause |
| `dio` | ^5.8.0 | MIT |
| `web_socket_channel` | ^3.0.2 | BSD-3-Clause |
| `connectivity_plus` | ^6.1.3 | BSD-3-Clause |
| `freezed_annotation` | ^2.4.4 | MIT |
| `json_annotation` | ^4.9.0 | BSD-3-Clause |
| `drift` | ^2.25.1 | MIT |
| `path_provider` | ^2.1.5 | BSD-3-Clause |
| `path` | ^1.9.1 | BSD-3-Clause |
| `shared_preferences` | ^2.5.2 | BSD-3-Clause |
| `flutter_secure_storage` | ^9.2.4 | BSD-3-Clause |
| `crypto` | ^3.0.6 | BSD-3-Clause |
| `flutter_contacts` | ^1.1.9 | MIT |
| `cached_network_image` | ^3.4.1 | MIT |
| `image_picker` | ^1.1.2 | Apache-2.0 |
| `file_picker` | ^8.3.7 | MIT |
| `just_audio` | ^0.9.46 | MIT |
| `record` | ^5.2.1 | BSD-3-Clause |
| `geolocator` | ^13.0.2 | MIT |
| `url_launcher` | ^6.3.1 | BSD-3-Clause |
| `flutter_webrtc` | ^0.12.5 | MIT |
| `firebase_core`, `firebase_messaging` | ^3.12.1 / ^15.2.4 | BSD-3-Clause |
| `flutter_local_notifications` | ^18.0.1 | BSD-3-Clause |
| `intl` | ^0.19.0 | BSD-3-Clause |
| `uuid` | ^4.5.1 | MIT |
| `collection` | ^1.19.1 | BSD-3-Clause |
| `libsignal_protocol_dart` | ^0.8.2 | **GPL-3.0** — see above |

`flutter_webrtc` bundles Google's WebRTC native libraries, which are
BSD-3-Clause with a separate patent grant. Firebase packages are wrappers around
the Firebase SDKs, whose own terms apply to the native artefacts they pull in.

---

## Keeping this accurate

A table written by hand goes stale the first time someone adds a package. The
package managers can both produce the real, transitively complete answer:

```sh
# Backend: every module in the build graph with its licence.
go install github.com/google/go-licenses@latest
go-licenses report ./... > licences-backend.csv

# Mobile: the notices Flutter itself shows under "Licenses", which is also
# what has to be surfaced in the app's about screen.
cd apps/mobile && flutter pub deps --json > licences-mobile.json
```

Run both before a release and reconcile any difference against this file. If a
new dependency is not permissive, it belongs in the encryption section's
company: recorded, with its consequence for distribution written out.
