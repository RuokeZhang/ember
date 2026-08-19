import { bootstrapSession, getCatalog, listEndpoints } from "./api";

export async function loadInitialData() {
  const session = await bootstrapSession();
  const [catalog, endpoints] = await Promise.all([getCatalog(), listEndpoints()]);
  return { session, catalog, endpoints };
}
