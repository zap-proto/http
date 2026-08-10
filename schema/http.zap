# zap-http — HTTP request/response semantics over the ZAP transport.
#
# Authored in ZAP schema language (whitespace-significant, no curly braces,
# field ordinals inferred from declaration order).
#
# ../wire.go and ../codec.go are the BINDING — there is no external
# codec dependency, codec.go is both encoder and decoder — and this
# file documents the logical shape they implement. Each struct below is
# an object whose fixed payload packs every text/bytes field into an
# 8-byte slot of {relOffset UInt32, length UInt32}, and scalars at
# their natural-width offset. A slot's relOffset is measured from THE
# SLOT'S OWN position; relOffset == 0 is NULL regardless of the length
# word, and an empty field keeps a zeroed {0,0} slot contributing no
# tail bytes. Slot offsets, from ../wire.go:
#
#   Request   method 0  target 8  proto 16  headers 24  body 32  trailer 40  (48 total)
#   Response  status 0  reason 8  proto 16  headers 24  body 32  trailer 40  (48 total)
#   Chunk     data   0                                                       ( 8 total)
#
# Byte order is little-endian everywhere INSIDE the frame. The only
# big-endian quantity on the connection is the transport's 4-byte
# length prefix that precedes each frame.
#
# The wire is one ZAP frame per HTTP message. The frame type (request
# vs response) rides in the message header flags (flags>>8), not a
# union struct. A request frame carries the request line + headers +
# body; a response frame carries status + headers + body. Both sides
# may carry trailers (RFC 9110 §6.5).
#
# Headers and trailers are LENGTH-PREFIXED PAIRS in a single bytes
# slot — not a JSON map, and not a nested struct list. A JSON encoder
# inside a zero-copy binary protocol bought escaping, sorting and a
# grammar nobody needed, and forced two implementations to be kept
# byte-identical; see ../wire.go's "Wire" note. The golden-wire tests
# pin the current bytes. Peers must agree on the version.
#
# Streaming is represented: a body too large for one frame is sent as
# FrameResponseHead (status + headers, no body), then zero or more
# FrameData chunks, then FrameEnd (optional trailers) — the shape
# HTTP/2 gets from HEADERS + DATA + END_STREAM. A NON-streamed body
# must still fit in one frame; the transport negotiates the maximum
# frame size on connection setup.

# The header block encoding, written out because it is a byte layout
# rather than a struct the schema language can express. `headers` and
# `trailer` below are Data slots holding exactly this, all integers
# little-endian:
#
#   count   UInt32          -- number of pairs that follow
#   pair*                   -- repeated `count` times:
#     keyLen   UInt32
#     key      keyLen bytes
#     valLen   UInt32
#     value    valLen bytes
#
# An EMPTY block writes no bytes at all and leaves a null {0,0} slot —
# it is not a `count=0` block. Decoders must treat a zero-length slot
# as "no headers" and stop; a block shorter than its own declared
# pairs is a truncated-header error, not a silent short read.
#
# RFC 9110 allows a name to repeat. There is no values list: a repeated
# name is simply REPEATED PAIRS, in the order sent, so callers still
# see what was actually on the wire without comma-concatenation.
#
# Some fields are FRAME-OWNED and must not appear as pairs, because the
# frame already carries the information and sending it would put two
# sources of truth on the wire: Content-Length (the body slot's length
# is authoritative) and, on requests, Host (reconstructed from the
# target). Encoders skip them; decoders ignore Content-Length and let
# the last Host value win.

# Request is one HTTP request. `target` is the request-target as it
# would appear on an HTTP/1.1 request line — origin-form (`/path?q=v`),
# absolute-form (`https://host/path`), authority-form (`host:port` for
# CONNECT), or asterisk-form (`*` for OPTIONS).
struct Request
  method  Text
  target  Text
  proto   Text
  headers Data
  body    Data
  trailer Data

# Response is one HTTP response. `status` is the numeric status code
# (200, 404, 503, …); `reason` is the optional reason phrase; the
# transport ignores the phrase for routing but preserves it for any
# caller that still parses it.
struct Response
  status  UInt16
  reason  Text
  proto   Text
  headers Data
  body    Data
  trailer Data

# Chunk is the body of a FrameData or FrameEnd frame: one bytes slot
# holding the chunk, or — for FrameEnd — the trailer pairs in the same
# encoding as `headers`. FrameEnd with a null slot terminates a stream
# that declared no trailers.
struct Chunk
  data Data

# Frame discrimination is carried by the ZAP message header flags
# (flags>>8), not a union struct (see ../wire.go). A Request object is
# the message root when the flag is FrameRequest; a Response object is
# the root when it is FrameResponse; a Chunk is the root for the two
# streaming data frames. There are five type IDs:
#
#   FrameRequest      0x01  -- Request is the root
#   FrameResponse     0x02  -- Response is the root, body in-frame
#   FrameResponseHead 0x03  -- Response is the root, body slot null,
#                              FrameData/FrameEnd follow
#   FrameData         0x04  -- Chunk is the root: one body chunk
#   FrameEnd          0x05  -- Chunk is the root: trailer pairs, or null
#
# A decoder that reads only single-frame replies must REFUSE 0x03 by
# name rather than return a body-less 200 — a truncated success is
# worse than a named error.
