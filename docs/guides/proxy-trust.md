# Proxy Trust

Credo ignores forwarded client metadata by default. Configure trusted proxy CIDR
ranges before using `X-Forwarded-*` or `X-Real-IP` values.

```go
app, err := credo.New(credo.WithTrustedProxies(
    "10.0.0.0/8",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "127.0.0.1/32",
    "::1/128",
))
```

Equivalent config:

```yaml
server:
  trusted_proxies:
    - 10.0.0.0/8
    - 172.16.0.0/12
    - 192.168.0.0/16
```

`WithTrustedProxies` overrides `server.trusted_proxies` when both are present.
Invalid CIDR values make `credo.New()` return an error.

Use the request helpers everywhere application code needs client metadata:

```go
scheme := ctx.Request().Scheme() // "http" or "https"
ip := ctx.Request().RealIP()     // original client IP
```

Trust model:

- If the immediate peer `RemoteAddr` is not trusted, all forwarded headers are ignored.
- `Scheme()` accepts only exact `http`, `https`, `on`, or `off` forwarded values.
- `RealIP()` walks `X-Forwarded-For` right-to-left and skips trusted proxy hops.
- `X-Real-IP` is used only when `X-Forwarded-For` has no usable IP.
- At most 32 `X-Forwarded-For` hops are inspected per request.

Framework middleware uses the same helpers:

- `middleware.Secure` uses `Request.Scheme()` for HSTS.
- `middleware.RateLimit` uses `Request.RealIP()` as its default key.
- Built-in and configurable access logs use `Request.RealIP()` for `remote_addr`.
