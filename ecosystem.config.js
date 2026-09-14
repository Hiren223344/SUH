// pm2 process definition for the router.
//
// Usage:
//   go build -o router.exe ./cmd/router
//   pm2 start ecosystem.config.js
//   pm2 save
//   pm2 startup   # then run the printed command once, as root, so pm2
//                 # itself (and this process) survives a reboot
//
// See README.md "Build & run" for config.yaml setup (listen port, API
// keys) before starting.
module.exports = {
  apps: [
    {
      name: "router",
      script: "./router.exe",
      args: "--config config.yaml",
      cwd: __dirname,
      interpreter: "none", // router.exe is a native binary, not a Node script
      exec_mode: "fork",
      instances: 1, // single-process by design: in-memory debt/breaker/TPM
      // state is per-instance; running more than one needs redis.enabled:
      // true (see README "Deliberate simplifications") so instances
      // converge, otherwise each instance's distribution fidelity drifts
      // independently.
      autorestart: true,
      restart_delay: 2000,
      max_restarts: 50,
      // Config hot-reloads itself on a 10s poll (internal/config/manager.go)
      // without dropping in-flight requests, so pm2 never needs to restart
      // this process for a config change.
      env: {
        // KIOSAPI_KEY: "set this in the real environment, not here",
      },
      out_file: "./logs/router.out.log",
      error_file: "./logs/router.err.log",
      merge_logs: true,
      time: true,
    },
  ],
};
