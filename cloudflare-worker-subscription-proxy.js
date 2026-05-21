const ORIGIN_BASE = "http://do.aigh.store";
const SUB_PREFIX = "/jbhd/customer-sub/";

export default {
  async fetch(request) {
    const url = new URL(request.url);

    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method Not Allowed", { status: 405 });
    }

    if (!url.pathname.startsWith(SUB_PREFIX)) {
      return new Response("Not Found", { status: 404 });
    }

    const token = url.pathname.slice(SUB_PREFIX.length);
    if (!/^[A-Za-z0-9_-]{6,64}$/.test(token)) {
      return new Response("Not Found", { status: 404 });
    }

    const originUrl = new URL(`${SUB_PREFIX}${token}`, ORIGIN_BASE);
    originUrl.search = url.search;

    const originResponse = await fetch(originUrl.toString(), {
      method: request.method,
      headers: {
        "User-Agent": request.headers.get("User-Agent") || "Subscription-Proxy",
        "Accept": request.headers.get("Accept") || "*/*",
      },
      cf: {
        cacheTtl: 0,
        cacheEverything: false,
      },
    });

    const headers = new Headers(originResponse.headers);
    headers.set("Content-Type", "text/plain; charset=utf-8");
    headers.set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0");
    headers.set("Pragma", "no-cache");
    headers.delete("Server");

    return new Response(originResponse.body, {
      status: originResponse.status,
      statusText: originResponse.statusText,
      headers,
    });
  },
};
