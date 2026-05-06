# zap-http — HTTP request/response semantics over the ZAP transport.
#
# The wire is one ZAP frame per HTTP message. A request frame carries
# the request line + headers + body; a response frame carries status +
# headers + body. Both sides may carry trailers (RFC 9110 §6.5).
#
# Streaming (chunked-style transfer) is reserved for a follow-up
# protocol slice and not represented here. v0.1 assumes the entire body
# fits in one frame; the transport (zap-proto/go) negotiates the
# maximum frame size on connection setup.

@0xa3d4b2e80f1c5a7e;

using Go = import "/go.capnp";
$Go.package("zaphttp_capnp");
$Go.import("github.com/zap-proto/http/internal/capnp");

# Header is one HTTP header name and its values. RFC 9110 allows the
# same name to appear multiple times; we represent that as a list of
# values rather than concatenating with commas, so callers see what
# was actually sent.
struct Header {
  name @0 :Text;
  values @1 :List(Text);
}

# Request is one HTTP request. `target` is the request-target as it
# would appear on an HTTP/1.1 request line — origin-form (`/path?q=v`),
# absolute-form (`https://host/path`), authority-form (`host:port` for
# CONNECT), or asterisk-form (`*` for OPTIONS).
struct Request {
  method @0 :Text;
  target @1 :Text;
  proto  @2 :Text;
  headers @3 :List(Header);
  body @4 :Data;
  trailer @5 :List(Header);
}

# Response is one HTTP response. `status` is the numeric status code
# (200, 404, 503, …); `reason` is the optional reason phrase; the
# transport ignores the phrase for routing but preserves it for any
# caller that still parses it.
struct Response {
  status @0 :UInt16;
  reason @1 :Text;
  proto @2 :Text;
  headers @3 :List(Header);
  body @4 :Data;
  trailer @5 :List(Header);
}

# Frame is the union the wire actually carries. Some sub-protocols may
# add Notify or Stream variants in future; today there are exactly two.
struct Frame {
  union {
    request @0 :Request;
    response @1 :Response;
  }
}
