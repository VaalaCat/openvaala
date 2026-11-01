import { QueryClient } from "@tanstack/react-query"

// 页面共用的 QueryClient：失败不自动重试，切回窗口不重取，不按浏览器在线状态排队
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        refetchOnWindowFocus: false,
        networkMode: "always",
      },
      // 离线直接返回请求错误，不排队等待网络恢复后重放写操作。
      mutations: { retry: false, gcTime: 0, networkMode: "always" },
    },
  })
}
