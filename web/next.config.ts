import type { NextConfig } from "next";

const isDevelopment = process.env.NODE_ENV === "development";
const backendOrigin =
  process.env.BKCLAW_BACKEND_ORIGIN || "http://127.0.0.1:18953";

const nextConfig: NextConfig = {
  ...(isDevelopment
    ? {
        skipTrailingSlashRedirect: true,
        async rewrites() {
          return [
            {
              source: "/api/:path*",
              destination: `${backendOrigin}/api/:path*`,
            },
            {
              source: "/v1/:path*",
              destination: `${backendOrigin}/v1/:path*`,
            },
          ];
        },
      }
    : { output: "export" as const }),
  trailingSlash: true,
  images: {
    unoptimized: true,
  },
};

export default nextConfig;
