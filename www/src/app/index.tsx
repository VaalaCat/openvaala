import { Navigate } from "react-router"
import { useServerConnection } from "@/components/server-connection-provider"
import { getServerPath } from "@/lib/paths"

// 入口页：连上 serve 后跳到 /s/<serverID>；连上之前由 ServerConnectionProvider 显示提示，不会渲染到这里
export function IndexPage() {
  const connection = useServerConnection()
  return <Navigate to={getServerPath(connection.getServerID())} replace />
}
