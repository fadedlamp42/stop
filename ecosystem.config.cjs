// pm2 ecosystem config for stop serve
// usage: pm2 start ecosystem.config.cjs

module.exports = {
  apps: [
    {
      name: "stop-serve",
      script: "./stop",
      args: "serve -p 8391",
      cwd: __dirname,
      interpreter: "none",

      autorestart: true,
      max_restarts: 10,
      min_uptime: "5s",

      watch: false,

      log_date_format: "YYYY-MM-DD HH:mm:ss",
      merge_logs: true,

      kill_timeout: 3000,
    },
  ],
};
