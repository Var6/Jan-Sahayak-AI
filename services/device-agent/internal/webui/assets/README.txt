The built citizen kiosk UI is copied into assets/dist/ by
scripts/dev/build-webui.sh (run by `make build`) and embedded into the
device-agent binary with go:embed. assets/dist/ is a build output and is
git-ignored; this file exists so the embed pattern always matches and the
agent compiles (serving a "UI not built" page) before the UI is built.
