# Internal Gotenberg service

This is a configuration example, not a deployed infrastructure project. Replace
`<VERSION>` and `<DIGEST>` with a verified Gotenberg Chromium image, and set your
SSH host, user and target path. The placeholders intentionally fail validation.
Check the [image variants](https://gotenberg.dev/docs/getting-started/installation#image-variants),
[Chromium settings](https://gotenberg.dev/docs/configuration) and
[health endpoint](https://gotenberg.dev/docs/system/get-health-check) for that release.

The existing `traefik-net.network` must be available to the rootless SSH user.
No host port, router or .env is required. A client on that network uses
`http://gotenberg-prod:3000`. Give test environments their own service name,
target directory and network alias. Let the image select its own container user;
Chromium needs writable temporary storage. Tune the resource and concurrency
values through load testing.

Create a separate Git repository for the actual infrastructure configuration,
commit and tag it, then preview `deploy --dry-run release prod` before releasing.
The Git tag versions this configuration; the image tag/digest identifies the
upstream software. PAM integration is a separate application change.
