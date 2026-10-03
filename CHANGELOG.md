# Changelog

## 0.1.0 (2026-10-03)


### Features

* a working demo: task demo, the hello example and the browser helper ([#17](https://github.com/ConfigButler/krm-foyer/issues/17)) ([8891215](https://github.com/ConfigButler/krm-foyer/commit/8891215b0c461b4a8436a927f4fa9ee1b26ab05d))
* bound requests and responses, end open watches with their session, and serve metrics ([#22](https://github.com/ConfigButler/krm-foyer/issues/22)) ([2a6f43c](https://github.com/ConfigButler/krm-foyer/commit/2a6f43c4033e5c77f338e3578685d44372e9a1cc))
* build streams on krm-stream 0.5.0, which maps the API server's answers and leaves retries to the browser ([#25](https://github.com/ConfigButler/krm-foyer/issues/25)) ([cdf1e23](https://github.com/ConfigButler/krm-foyer/commit/cdf1e23a6b1848287c768fca4a66354d209addc4))
* build streams on krm-stream 0.6.0, which refuses redirects and stops reopening early-ending watches ([#26](https://github.com/ConfigButler/krm-foyer/issues/26)) ([c746c0c](https://github.com/ConfigButler/krm-foyer/commit/c746c0c4358e79f7d8082539bc846830bc12d8f4))
* interruption pages for browser navigations ([#12](https://github.com/ConfigButler/krm-foyer/issues/12)) ([243da5c](https://github.com/ConfigButler/krm-foyer/commit/243da5c84c03e0f4e7add96cc30f316775e79667))
* live notes with krm-stream: user-authenticated streams, closed with their session ([#23](https://github.com/ConfigButler/krm-foyer/issues/23)) ([5bf5dfb](https://github.com/ConfigButler/krm-foyer/commit/5bf5dfb82223ca29f0aa865c66d6a1c3d7abb1db))
* OIDC login with PKCE, state and nonce, and /k8s behind sessions ([#11](https://github.com/ConfigButler/krm-foyer/issues/11)) ([a514dc8](https://github.com/ConfigButler/krm-foyer/commit/a514dc89783a2cf8ddaa5e0ddb6ffc9f0378b375))
* proxy /k8s with the user's token, path checking and upstream response rules ([#9](https://github.com/ConfigButler/krm-foyer/issues/9)) ([3355231](https://github.com/ConfigButler/krm-foyer/commit/3355231ce709824cb867a62dec428cd1b9bb4ffc))
* server-side sessions with CSRF and same-origin checks ([#10](https://github.com/ConfigButler/krm-foyer/issues/10)) ([047f10c](https://github.com/ConfigButler/krm-foyer/commit/047f10cf8ea3f2447d37137efa1d223d42f5b0ae))


### Bug Fixes

* keep issuer error text out of the logs, compare e2e bodies, and keep sessions free of HTTP ([#16](https://github.com/ConfigButler/krm-foyer/issues/16)) ([da7efd8](https://github.com/ConfigButler/krm-foyer/commit/da7efd8cc92e0634f001355c60780c5f91d1e4bc))
* resend only krm-foyer's own CSRF refusals, log ID tokens by cause, and encrypt every hop of the demo ([#21](https://github.com/ConfigButler/krm-foyer/issues/21)) ([d7bd208](https://github.com/ConfigButler/krm-foyer/commit/d7bd208b61e8e7862d61e9d0e01d25d553e4b119))


### Documentation

* defer proxy subresources, qualify revocation, and specify decompression ([be2b463](https://github.com/ConfigButler/krm-foyer/commit/be2b46385cdbc17e90232929a949f6f70738de82))
* restructure around a vision and roadmap, and start without an allowlist ([#7](https://github.com/ConfigButler/krm-foyer/issues/7)) ([702aa65](https://github.com/ConfigButler/krm-foyer/commit/702aa657e4369a481035d616a02557778f12462d))
