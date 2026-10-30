# OpenAI Proxy

A self-contained application for proxying an OpenAI-compatible API endpoint. This is to allow one OpenAI endpoint to be shared between multiple users, with per-user usage accounting, logging and filtering in place. Designed to be used by schools and in corporate environments as a gateway server in front of an AI inference service of some sort - maybe a commercially available service (OpenRouter, etc), or possibly a local dedicated AI server.

Distributed as a single Go executable containing application logic, an embedded Bootstrap web user interface and interactive OpenAPI documentation.

## Features

- **Single Executable Deployment**: Uses Go's `embed` package to bundle the frontend HTML / CSS / JavaScript assets, documentation and OpenAPI specifications into a single binary.
- **Offline Operation**: Designed to be able to operate without internet access; all UI libraries and documentation resources are served locally.
- **Works on Desktop and Server**:
  - **Desktop Mode**: Ideal for local home desktop use on pretty much any platform ([Windows](https://www.microsoft.com/windows/) / [Windows Server](https://www.microsoft.com/windows-server), [MacOS](https://en.wikipedia.org/wiki/MacOS), and [Linux](https://en.wikipedia.org/wiki/Linux), including both the [Raspberry Pi](https://www.raspberrypi.com/) (and other single-board computers) and [ChromeOS](https://chromeos.google/intl/en_uk/products/chromeos-flex/) ([Crostini](https://chromeos.dev/en/linux))). Running the executable on your desktop machine should give you a localhost-only server and automatically launch your default web browser to display the user interface.
  - **Server Mode**: Suitable for multi-user deployment behind authenticating reverse proxies ([Pangolin](https://pangolin.net/), [Cloudflare Tunnel](https://developers.cloudflare.com/tunnel/), [Authelia](https://www.authelia.com/), [Tailscale](https://tailscale.com/), [etc](https://github.com/anderspitman/awesome-tunneling)). Authenticates users via incoming proxy headers (`X-Forwarded-User`, `Remote-User`, `Pangolin-User`, etc.). As a single, statically linked Go binary with no external dependencies, it can be run inside a very minimal (`scratch`) container environment.
- **Built for use by humans and AI agents**: With built-in documentation and Swagger UI, documentation should be easy to understand for both humans and AI agents.
- **Built for extending by humans and AI agents**: Point an AI coding agent at AGENTS.md (or read it yourself) and use this project as a basis for your own app, the project structure is kept deliberately simple.

---

## Running

### Desktop

Simply download and run the executable for your platform from the [project homepage](https://sansay.co.uk/openai-proxy/).

### Server

You can run Go App behind an authenticating proxy server - the proxy server handles HTTPS, authenticates the user and passes the username to the application via an HTTP header. As the executable is compiled with CGO disabled (CGO_ENABLED=0) and the proxy server is dealing with HTTPS, the container environment can use the `scratch` (completely empty) container image. For instance, if you were using Pangolin as your authenticating server, you would add a basic Dockerfile:

```
# Note: the "scratch" image is 0 bytes, it doesn't have tools like chmod, so there's some extra steps
# needed to get executable files inside a "scratch" image. We need to build a "downloader" image...
FROM alpine:latest AS downloader
RUN apk add --no-cache curl && \
    curl -L https://www.sansay.co.uk/openai-proxy/openai-proxy-linux-amd64 -o /openai-proxy && \
    chmod +x /openai-proxy

# ...then use that to build the actual image we want.
FROM scratch
COPY --from=downloader /openai-proxy /openai-proxy
```
And to docker compose, add something like:
```
goapp:
    image: GO_APP_IMAGE
    command: /openai-proxy -mode=server -port=8080 -base-path=/openai-proxy
    container_name: goapp
    restart: unless-stopped
```
From the Pangolin control panel you would then create a resource, possibly with a prefix ("openai-proxy" or whatever you have named your derived app) using whatever authentication and access controls you like, that pointed at that container (`goapp:8080`). If you do use a prefix for the resource, be sure to add that prefix as the "-base-path" option when running the server.

## Building

Clone the repository:

```
git clone https://github.com/dhicks6345789/openai-proxy.git
```

And run build:

```
cd openai-proxy
bash build.sh build-all
```

This will compile the executables for all platforms.

You can build executables and generate documentation, including Swaggo's interactive API documentation, and copy the lot directly to somewhere they can be served as a web site to act as a project homepage using "build.sh dist". You just need to specify the path you want the files to go to, e.g.:

```
bash build.sh dist ~/www/openai-proxy
```
