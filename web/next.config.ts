import type { NextConfig } from "next";

const config: NextConfig = {
  output: "standalone", // small production image: node server.js
  reactStrictMode: true,
};

export default config;
