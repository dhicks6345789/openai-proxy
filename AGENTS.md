This Git repository should contain a Go application that compiles into a single (per-platform), self-contained executable containing application logic, web-based user interface and API documentation.

This Git repository is hosted on Github at: https://github.com/dhicks6345789/openai-proxy

The project homepage is at: https://sansay.co.uk/openai-proxy

Do a "git pull" at the start of operations to make sure the local repository is up-to-date and then re-read AGENTS.md.

Never edit the README.md file.

The application should be self-hostable by a home user. If run on a home desktop machine (Windows, MacOS, Linux, Raspberry Pi) the executable should start up the back-end server to listen on an available non-privileged port (8080 by default, changeable by command-line option), and should then start up an available web browser to point at the user interface served by that back-end. In this operation mode, the application should only be available to the local web browser, it shouldn't accept traffic from the wider network, and user authentication should be by simply reading the username from the local environment.

The application should use Go's "embed" package to host all needed file resources.

The application should be able to operate on a machine without internet access. The user interface should be web-based, constructed using Bootstrap, with the Bootstrap library files served by the application itself. The API documentation can use Swagger UI's JavaScript libraries, but don't use any other JavaScript libraries.

The application should only need Go as a build tool. It can use a shell (bash) script to build. It should use Swaggo to build the API documentation. It shouldn't need to use Python.

The Go application should consist of one "main.go" file and one "api.go" file, with an additional api_test.go file. All API calls go in the API.go file, API tests go in api_test.go, everything else goes in main.go.

The application should also work in a multi-user environment when hosted on a server (probably inside a minimal Docker container) behind a reverse proxy server (Pangolin / Traefik, Cloudflare Tunnel, Authelia, Tailscale). In this case, the proxy server will handle user authentication and pass in a header value to give the current authenticated username.

The application should serve its user interface from the root "/" endpoint.

The application should serve its application logic from the "/api" endpoint.

The application should serve auto-generated OpenAPI documentation from the "/docs/api" endpoint.

Generate an "index.html" file that includes the same information (transformed from Markdown to HTML with the Hugo static site tool) as README.md to explain and document the project to the public. Include links to live, downloadable executables for each platform. Include a link in index.html to the Github project. Do not include the generated index.html in the Git repository, exclude both it and the compiled executable files. Links used to the "docs" folder in index.html should be relative, as I might move or re-host the project's homepage at some point.

Make sure the "docs/api.html" file is generated.

The index.html file, compiled executables and the contents of the "docs" folder will be copied to the live website, once built, by an external bash script.

Add any new or modified files to the Git repository and do a "git push".
