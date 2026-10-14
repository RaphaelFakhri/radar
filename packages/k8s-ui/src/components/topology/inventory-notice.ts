import type { Topology } from '../../types'

export function topologyViewLimitMessage(inventories: Topology['incompleteInventories']): string {
  const loading = inventories?.some(inventory => inventory.state === 'pending')
  const scoped = inventories?.some(inventory => inventory.state === 'uncovered')
  if (loading && scoped) return 'Some resource data is still loading; Radar watches only selected namespaces.'
  if (loading) return 'Some resource data is still loading.'
  if (scoped) return 'Radar watches only selected namespaces.'
  return 'Large retained run history is summarized to keep the graph readable.'
}
