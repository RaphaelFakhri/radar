import { describe, expect, it } from 'vitest'
import { topologyViewLimitMessage } from './inventory-notice'

describe('Topology view-limit notice', () => {
  it('describes loading instead of retained history', () => {
    expect(topologyViewLimitMessage([{ resource: 'pods', state: 'pending', message: 'Pod data is still loading' }])).toBe('Some resource data is still loading.')
  })
  it('describes a namespace limit instead of an outage or retained history', () => {
    expect(topologyViewLimitMessage([{ resource: 'pods', state: 'uncovered', message: 'Pod data may be incomplete' }])).toBe('Radar watches only selected namespaces.')
  })
  it('keeps both independent limits visible', () => {
    expect(topologyViewLimitMessage([{ resource: 'pods', state: 'pending', message: '' }, { resource: 'services', state: 'uncovered', message: '' }])).toBe('Some resource data is still loading; Radar watches only selected namespaces.')
  })
  it('preserves the existing history-only explanation', () => {
    expect(topologyViewLimitMessage(undefined)).toBe('Large retained run history is summarized to keep the graph readable.')
  })
})
