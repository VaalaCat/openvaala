# AWP v1

World nodes publish a directory over a control WebSocket and exchange end-to-end encrypted messages over separate data WebSockets. A local bridge connects a node to an external session host over stdio.

## L1 Control and relay pairing

Control messages are JSON text frames with `type` and optional `payload`. Binary JSON fields use standard Base64.

| Endpoint under `/world/v1/` | Wire behavior |
| --- | --- |
| `connect` | A domain member answers a challenge and sends `node.announce` with `entries: {direct_urls, relays}` and `targets`. The server sends ACL-filtered `directory` snapshots with signing keys, entries, targets, `online`, `can_send` and `can_receive`. Each snapshot also includes `domains`, the trusted domain cards. Control frames include `ping`, `pong` and `node.revoked`. |
| `relay/attach` | An additional relay challenges the attaching node. Both attach and relay-enabled connect send `relay.attached` after registration. |
| `relay/dial?target=<key>` | The target is an Ed25519 public key in unpadded base64url. The relay notifies its attachment with `relay.incoming` and a random 128-bit `connection_id` in unpadded base64url. |
| `relay/accept?connection_id=<id>` | The ID pairs one accept with one dial within 10 seconds. The relay forwards binary messages in both directions with message boundaries intact. An unavailable target, expired pairing or unknown ID closes with code 4004 and reason `world_target_unreachable`. |

The server sends `challenge` with 32 random bytes. The node returns `challenge.response` with `public_key`, `node_id`, `timestamp` (Unix seconds) and an Ed25519 `signature`. The signed bytes begin with the NUL-terminated UTF-8 label `openvaala-world-connect/v1` for connect or `openvaala-world-relay-attach/v1` for attach. The challenge, node ID bytes and 8-byte big-endian timestamp follow, each prefixed with its 8-byte big-endian length.

An authenticated member asks its control plane to resolve a foreign node with `domain.lookup` carrying `{request_id,domain,node}`. The control plane dials the domain from its own trust table and returns `domain.lookupResult` on the same connection with the matching ID and either `{domain_key,member:{public_key,owner,entries}}` or `{code,error}`. Disconnect ends outstanding requests.

Issuing a join code returns `token` once, in the form `<http(s)-control-base>#<random-secret>`. The base preserves the deployment prefix. The control plane uses its first advertised direct entry, or its local listener when no direct entry is configured. Members run `openvaala world join '<join-code>'`. The client posts the complete code as `join_token` to `/world/v1/nodes/register` at the embedded base. Only the secret is hashed for redemption. Members can edit the address before `#` to reach the same control plane.

Administrators exchange cards of the form `domain#base64url(JSON{domain,public_key,entries})` (unpadded outer encoding, standard Base64 public key). Under `/world/v1/admin/domains`, GET lists the local card and the peer domains with their status. POST accepts `{card}`, POST to `/:domain/accept` or `/:domain/reject` decides a received invitation, and DELETE removes `/:domain` in any state. The operator RPC methods are `world/admin/listDomains`, `addDomain`, `acceptDomain`, `rejectDomain` and `deleteDomain` under the same `world/admin/` prefix. Each decision takes `{domain}`. A name already bound to another key returns HTTP 409 with `world_domain_name_taken`. An empty self card calls for configuring an advertised direct URL or a public relay.

A removed member receives `node.revoked`. On reconnect, the control plane sends the same frame with `code: "world_node_revoked"` and closes the connection. The node reports `revoked: true` through `world/getStatus` and returns `world_node_revoked` on send. Rejoining with a new join code restores membership.

### Public relay instances

A relay entry can be an instance or a load balancer. At its base path, `GET /world/v1/relay/instances` over HTTP(S) returns `{"instances":["https://a.example","https://b.example"]}` with `Access-Control-Allow-Origin: *`. A standalone public relay with no configured instances returns `{"instances":[]}`. A 404 also denotes a single relay. Cards, directory entries and remote access links publish entry addresses.

For each instance, compute SHA-256 over the UTF-8 bytes of `openvaala-relay-hrw/v1`, one zero byte, the instance address exactly as published in UTF-8, one zero byte, and the raw 32-byte Ed25519 public key of the attaching node or domain. Sort by the unsigned big-endian 32-byte digest in descending order, breaking ties by ascending address bytes. Select the first two instances, or all when fewer than two exist.

For each configured public relay entry, the attaching node or domain fetches the list on startup, before reattaching and every 10 minutes. It attaches to the new selection before dropping superseded connections. An empty list selects the entry itself. On lookup failure it keeps the previous selection, defaulting to the entry on first use. The home control connection goes directly to `/world/v1/connect` and also serves as the attachment when the home relay is enabled.

After direct attempts, dialers look up each entry in order. They cache successful lists, empty lists and 404 responses by entry for one minute. Discovery has a five-second timeout. On an empty list or failed lookup the dialer tries the entry itself. Otherwise it dials only the selected instances, in score order, allowing 15 seconds per instance.

## L2 End-to-end channel

Direct connections use `/world/v1/direct?purpose=<purpose>&target=<key>`. Direct and paired connections use the same binary handshake. Let `P` be the UTF-8 bytes of `openvaala-world-e2ee/v2`. Fields below are concatenated without length prefixes.

- **Hello** contains version `0x02`, purpose (`1=world`, `2=rpc`, `3=domain`), and identity flag (0 or 1), each one byte. Next come the 16-byte channel ID, the expected responder Ed25519 public key and the initiator ephemeral X25519 public key (32 bytes each). Flag 1 adds the initiator Ed25519 and static X25519 public keys, then an Ed25519 signature over `P || unsigned hello`. Flag 0 is valid only for RPC.
- **Ready** contains the responder ephemeral X25519 public key and, for flag 1, its static X25519 public key. The responder appends its Ed25519 signature over `P || complete hello || unsigned ready`. All signatures are 64 bytes.
- **Keys** derive from HKDF-SHA256 with channel ID as salt and `P || complete hello || complete ready` as info. Input is the ephemeral DH secret followed, for flag 1, by the static DH secret. Output is two 32-byte AES-256-GCM keys, initiator-to-responder first.
- **Data** is an 8-byte little-endian sequence number followed by ciphertext and a 16-byte tag. Each direction starts at 1 and increments by one. The nonce is four zero bytes followed by the sequence bytes. AAD is the channel ID. Maximum plaintext is 16 MiB. A receiving World node verifies the first encrypted message before registering the inbound channel.

For the `rpc` purpose, the initiator sends `{"token":"…"}` as its first encrypted frame. The responder returns encrypted `{"ok":true}` on success or `{"error":"unauthorized"}` before closing on rejection. After success, the client sends the runtime JSON-RPC `initialize` request over the same encrypted channel.

Remote access URLs use `<page-origin>/web/v1/#world=<base64url JSON>&token=<URL-encoded Runtime token>`. The unpadded payload carries the standard Base64 Ed25519 `public_key`, dial `entries`, and optional display `name`, `node` and `domain`. An independent Runtime can attach to a public relay with its local identity. Clients authenticate through the encrypted RPC channel. An older link without a token prompts for credentials. Browsers save the target by public key and clear the fragment after import.

The operator RPC `server/resetToken` accepts `{}` and replies with `{token}`. The Runtime persists the replacement and disconnects connections authenticated with the previous token. Reconnect and read `world/getStatus.remote_access_url` for the new link.

The `domain` purpose authenticates the initiating control plane's signing key. Each channel carries an encrypted trust exchange or member lookup.

| Operation | Request and handling |
| --- | --- |
| `trust_request` | `{"op":"trust_request","card":"home#…"}`. The card's public key must match the handshake signer. Importing a card creates a `pending_outbound` record and sends the local card. The recipient records `pending_inbound`. If both sides have imported the other's card, they become `mutual`. A peer with a saved decision returns its `trust_response` on the same channel. |
| `trust_response` | `{"op":"trust_response","domain":"home","decision":"accepted"}` or `"rejected"`. The recipient updates the matching `pending_outbound` row for that domain and signer to `mutual` or `rejected`. The administrator's decision takes effect locally before transmission. |
| `lookup` | `{"op":"lookup","node":"bob"}`. Only a `mutual` signer may receive a member's `{public_key,owner,entries}` or a `world_peer_not_found` response. |

A trust exchange returns an encrypted `{}` when there is no decision to send. A pending invitation survives an unreachable peer. Once the domain channel is authenticated, a responder with an outstanding invitation sends its `trust_request` immediately. If the caller has sent a trust operation, that operation also answers the invitation. A caller requesting a lookup returns its saved decision or `{}` before reading the member result. Only mutual domains appear in member directories. Sending to any other domain returns `world_domain_not_trusted`.

For the `world` purpose, the decrypted payload is one UTF-8 JSON envelope with `kind`, `message_id`, `thread_id`, `from`, `to`, and `hop_limit`. Each address is an object with `node`, `domain`, and optional `target`.

| Kind | Additional fields and behavior |
| --- | --- |
| `message` | `body`, optional `subject`. The recipient persists the message before acknowledging it and processes each thread in receive order. |
| `reply` | `in_reply_to` identifies the original message. `body` is the reply. The original sender persists the reply receipt and acknowledges it. |
| `ack` | `in_reply_to` identifies the acknowledged message or reply. `status` is `accepted`, `running`, `completed`, `failed`, or `not_found`. `body` can carry the reply or failure text. |
| `status_query` | `in_reply_to` identifies a message or reply whose acknowledgement is uncertain. The recipient returns its saved status or `not_found`. The sender retransmits on `not_found` for an outstanding query. |

The recipient checks local senders against the directory signing key and `can_receive`. Foreign senders require a trusted domain, a verified member key matching the handshake, and a public explicit or default target for a new thread. A foreign message in an existing thread may reach a private target when the local thread belongs to the same remote node. Successful lookups remain in memory for one hour. Removing or replacing a domain key invalidates its cached identities when the node receives the snapshot.

A failed ACK carries `error_code` and the error text in `body`; the outbox exposes `error_code` and `error_text`. Unreachable peers and unverified identities can be retried. An untrusted domain, a known missing destination, a private target or an ACL denial ends the send. A failed processing ACK with `world_agent_no_model` means the message was delivered but the receiving Agent could not run. Replies use the original connection. After a disconnect the sender can recover its saved result with a status query on a newly authenticated connection.

New messages start with a hop limit of 5. A send from a session bound to an inbound thread inherits that binding's remaining limit minus one. A limit of zero or less is rejected. World limits request and reply bodies to 512 KiB before JSON encoding.

## L3 Local bridge

Start `openvaala bridge --config-file path/to/world.yaml`. The bridge uses the configured `world.node` identity settings, `default_target`, and `advertise_targets`. Targets belong to the external host. This command has no HTTP listener and does not advertise direct URLs.

Each newline-terminated UTF-8 line is one JSON-RPC 2.0 object. The bridge reads stdin, writes protocol frames to stdout, and sends console logs to stderr. A frame may contain up to 8 MiB excluding the newline. Use smaller message pages if a response exceeds that limit. Batch arrays are unsupported.

Both parties can make requests concurrently. A frame with `method` and `id` is a request. A frame with `method` and no `id` is a notification and receives no response. A response has the request ID and either `result` or `error`. Each party matches responses only against its own pending IDs, so the same ID can be used simultaneously in both directions.

```json
{"jsonrpc":"2.0","id":1,"method":"world/initialize","params":{"protocol_version":1,"host_name":"example-host"}}
{"jsonrpc":"2.0","id":1,"result":{"protocol_version":1,"node_addr":"home@example.test","default_target":"assistant","targets":[{"name":"assistant","visibility":"domain"}]}}
```

The host must be ready to serve callbacks before sending `world/initialize`. The bridge starts its node after writing the successful handshake response. Recovery runs then, including queued incoming messages and pending outbound notifications. EOF ends the session. Write failure or cancellation closes the streams, releases pending calls, and stops the node.

### Host to bridge

Method parameters are objects. A trailing `?` marks an optional field.

| Method | Parameters | Result |
| --- | --- | --- |
| `world/initialize` | `protocol_version: 1`, `host_name?` | `protocol_version`, `node_addr`, `default_target`, `targets` |
| `world/status` | Empty | `enabled`, `node_addr`, `server_url`, `connected`, `pending_outbox_count` |
| `world/peers` | Empty | `peers`, the contact list with `address`, `node_name`, `online`, optional `description`, and visible `targets` |
| `world/send` | `to`, `message`, `subject?`, `thread_id?`, `source_conversation_id?`, `wait?`, `notify?` | Saved `WorldMessage` |
| `world/message` | `message_id`, `source_conversation_id?` | Exact `WorldMessage`. Supplying a source restricts the result to an outbound record from that session. |
| `world/messages` | `direction?`, `peer_addr?` (array of exact addresses), `kind?`, `status?`, `conversation_id?`, `thread_id?`, `limit?`, `cursor_id?` | `items`, optional `next_cursor: {id}`. IDs sort descending. Pass `next_cursor.id` as the next `cursor_id`. The default limit is 50 and the maximum is 200. |
| `world/cancel` | `message_id` | Cancelled `WorldMessage`. Only an outbound `pending_offline` message can be cancelled. |

`WorldMessage` is the stored view, including message and thread IDs, direction, peer address, body, status, and available reply, error, or session fields. `world/send` defaults to `wait: true` and `notify: false`. An offline recipient returns `pending_offline` immediately. An online send waits up to two minutes and can return an in-progress record. With `notify: true` and a source session, asynchronous completion is delivered through `world/notify`.

### Bridge to host

| Method | Parameters | Result |
| --- | --- | --- |
| `world/open` | `thread_id`, `target`, `peer_addr`, `title`, `existing_session_id?` | `session_id` |
| `world/deliver` | `session_id`, `message_id`, `thread_id`, `from`, `target`, `subject?`, `body`, `hop_limit` | `reply`, or a JSON-RPC error when execution fails |
| `world/notify` | `session_id`, `message_id`, `thread_id`, `peer_addr`, `status`, `reply_body?`, `error_text?` | Empty object. The callback has a 30-second timeout. A failure leaves the notification pending for a subsequent 15-second retry tick. |
| `world/event` | `event`, `data` | Notification. Events are `status_changed`, `peers_changed`, and `message_updated`. Query the node for authoritative state. |

The Runtime method `world/listPeers` also returns `recent`, one `{address,thread_id}` per distinct peer from the 20 most recently active foreign threads, in descending activity order.

The node saves the returned session ID and the inbound hop limit before invoking `world/deliver`. The host may send a nested `world/send` during delivery using this session ID as `source_conversation_id`. A host should deduplicate `world/notify` by `message_id`, since its acknowledgement can be lost after processing.

### Errors

| Code | Meaning |
| --- | --- |
| `-32700` | Invalid JSON |
| `-32600` | Invalid request or repeated initialization |
| `-32601` | Unknown method |
| `-32602` | Invalid parameters or unsupported handshake version |
| `-32000` | World operation failed. `error.data.code` carries the World error code. |
| `-32001` | Initialization required |
| `-32002` | Frame exceeded the limit. Oversized input closes the stdio session. |

World operation codes include `invalid_world_address`, `world_peer_not_found`, `world_target_not_exposed`, `world_hop_limit_exceeded`, `world_message_not_found`, `world_outbox_not_cancellable`, and `world_request_failed`. Business parameter errors use `-32602` with `error.data.code: "invalid_arguments"`.
