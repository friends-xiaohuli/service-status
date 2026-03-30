# Vercel Static Deploy

上传此目录到 Vercel 后即可部署。

部署前请在 Vercel 项目中设置：

- `Root Directory` = `vercel-static`
- 环境变量 `STATUS_API_URL` = `http://101.43.43.100:8080/api/status`

部署完成后，前端只会请求当前站点下的 `/api/status`，不会在浏览器里直接暴露源 IP 和端口。
