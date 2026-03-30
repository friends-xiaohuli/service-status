function jsonResponse(body, init = {}) {
  const headers = new Headers(init.headers || {});
  headers.set('content-type', 'application/json; charset=utf-8');
  headers.set('cache-control', 'no-store');

  return new Response(JSON.stringify(body), {
    ...init,
    headers,
  });
}

export async function GET() {
  const upstreamURL = process.env.STATUS_API_URL;
  if (!upstreamURL) {
    return jsonResponse({
      error: '全局服务器异常',
      detail: 'missing STATUS_API_URL',
    }, {
      status: 500,
    });
  }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 8000);

  try {
    const upstreamResp = await fetch(upstreamURL, {
      method: 'GET',
      headers: {
        'accept': 'application/json',
      },
      cache: 'no-store',
      signal: controller.signal,
    });

    if (!upstreamResp.ok) {
      return jsonResponse({
        error: '全局服务器异常',
        detail: `upstream returned ${upstreamResp.status}`,
      }, {
        status: 404,
      });
    }

    const contentType = upstreamResp.headers.get('content-type') || 'application/json; charset=utf-8';
    const bodyText = await upstreamResp.text();
    return new Response(bodyText, {
      status: upstreamResp.status,
      headers: {
        'content-type': contentType,
        'cache-control': 'no-store',
      },
    });
  } catch (error) {
    const detail = error && error.name === 'AbortError'
      ? 'upstream timeout'
      : 'upstream unreachable';

    return jsonResponse({
      error: '全局服务器异常',
      detail,
    }, {
      status: 404,
    });
  } finally {
    clearTimeout(timer);
  }
}
