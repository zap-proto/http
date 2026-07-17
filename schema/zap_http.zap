# zap-http — HTTP request/response semantics over the ZAP transport.
#
# Authored in ZAP schema language (whitespace-significant, no curly braces,
# field ordinals inferred from declaration order).
#
# This is the source of truth for the ZAP-HTTP wire format. The Go
# codec is hand-written against the github.com/zap-proto/go runtime in
# ../wire.go — each struct below maps to a zap-proto/go object whose
# fields are packed at fixed 8-byte-aligned slots (text/bytes) or
# natural-width scalar offsets. The field offsets in ../wire.go are the
# binding; this file documents the logical shape they implement.
#
# The wire is one ZAP frame per HTTP message. The frame type (request
# vs response) rides in the message header flags (flags>>8), not a
# union struct. A request frame carries the request line + headers +
# body; a response frame carries status + headers + body. Both sides
# may carry trailers (RFC 9110 §6.5). Headers and trailers are encoded
# as a JSON map<Text, List(Text)> rather than a nested struct list,
# matching http.Header's native shape losslessly.
#
# Streaming (chunked-style transfer) is reserved for a follow-up
# protocol slice and not represented here. v0.1 assumes the entire body
# fits in one frame; the transport (zap-proto/go) negotiates the
# maximum frame size on connection setup.

# Header is one HTTP header name and its values. RFC 9110 allows the
# same name to appear multiple times; we represent that as a list of
# values rather than concatenating with commas, so callers see what
# was actually sent.
struct Header
  name Text
  values List(Text)

# Request is one HTTP request. `target` is the request-target as it
# would appear on an HTTP/1.1 request line — origin-form (`/path?q=v`),
# absolute-form (`https://host/path`), authority-form (`host:port` for
# CONNECT), or asterisk-form (`*` for OPTIONS).
struct Request
  method  Text
  target  Text
  proto   Text
  headers List(Header)
  body    Data
  trailer List(Header)

# Response is one HTTP response. `status` is the numeric status code
# (200, 404, 503, …); `reason` is the optional reason phrase; the
# transport ignores the phrase for routing but preserves it for any
# caller that still parses it.
struct Response
  status  UInt16
  reason  Text
  proto   Text
  headers List(Header)
  body    Data
  trailer List(Header)

# Frame discrimination is carried by the ZAP message header flags
# (flags>>8), not a union struct: FrameRequest=0x01 tags a Request,
# FrameResponse=0x02 tags a Response (see ../wire.go). A Request object
# is the message root when the flag is FrameRequest; a Response object
# is the root when it is FrameResponse. Some sub-protocols may add
# Notify or Stream type IDs in future; today there are exactly two.
