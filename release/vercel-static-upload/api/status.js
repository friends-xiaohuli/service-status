import { gzipSync } from 'node:zlib';

function jsonResponse(body, init = {}) {
  const headers = new Headers(init.headers || {});
  headers.set('content-type', 'application/json; charset=utf-8');
  headers.set('cache-control', 'no-store');
  return new Response(JSON.stringify(body), { ...init, headers });
}

function buildUpstreamURL(baseURL, requestURL) {
  const upstream = new URL(baseURL);
  for (const [key, value] of requestURL.searchParams.entries()) {
    upstream.searchParams.append(key, value);
  }
  return upstream;
}

function maybeCompress(request, text, headers) {
  const acceptEncoding = request.headers.get('accept-encoding') || '';
  if (!acceptEncoding.includes('gzip') || text.length < 512) {
    return text;
  }
  headers.set('content-encoding', 'gzip');
  return gzipSync(Buffer.from(text));
}

function healthResponse() {
  return jsonResponse({
    status: 'ok',
    message: '请求正常',
  }, {
    status: 200,
    headers: {
      'cache-control': 'public, max-age=15, stale-while-revalidate=45',
    },
  });
}

export default {
  async fetch(request) {
    const upstreamURL = process.env.STATUS_API_URL;
    const currentURL = new URL(request.url);
    const view = currentURL.searchParams.get('view') || '';

    if (view !== 'summary' && view !== 'history') {
      return healthResponse();
    }

    if (!upstreamURL) {
      return jsonResponse({
        error: '全局服务器异常',
        detail: 'missing STATUS_API_URL',
      }, { status: 500 });
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);

    try {
      const upstream = buildUpstreamURL(upstreamURL, currentURL);
      const upstreamResp = await fetch(upstream, {
        method: 'GET',
        headers: {
          accept: 'application/json',
          'if-none-match': request.headers.get('if-none-match') || '',
        },
        cache: 'no-store',
        signal: controller.signal,
      });

      const headers = new Headers();
      headers.set('cache-control', upstreamResp.headers.get('cache-control') || 'no-store');
      headers.set('vary', 'Accept-Encoding, If-None-Match');

      const contentType = upstreamResp.headers.get('content-type') || 'application/json; charset=utf-8';
      headers.set('content-type', contentType);

      const etag = upstreamResp.headers.get('etag');
      if (etag) {
        headers.set('etag', etag);
      }

      if (upstreamResp.status === 304) {
        return new Response(null, { status: 304, headers });
      }

      if (!upstreamResp.ok) {
        return jsonResponse({
          error: '全局服务器异常',
          detail: `upstream returned ${upstreamResp.status}`,
        }, {
          status: upstreamResp.status,
          headers,
        });
      }

      const bodyText = await upstreamResp.text();
      const body = maybeCompress(request, bodyText, headers);
      return new Response(body, {
        status: upstreamResp.status,
        headers,
      });
    } catch (error) {
      const detail = error && error.name === 'AbortError'
        ? 'upstream timeout'
        : 'upstream unreachable';

      return jsonResponse({
        error: '全局服务器异常',
        detail,
      }, { status: 504 });
    } finally {
      clearTimeout(timer);
    }
  },
};
