// @vitest-environment jsdom
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveDebugSection } from './LiveDebugSection'
import { RadarFeatureUnsupportedError } from '../../../api/radarFeatures'

const mock = vi.hoisted(() => ({
  status: { data: { state: 'installed' }, isLoading: false, error: undefined } as any,
  start: vi.fn(), stop: vi.fn(), fetch: vi.fn(), query: vi.fn(),
}))
vi.mock('@tanstack/react-query', () => ({ useQuery: (options: any) => { mock.query(options); return mock.status } }))
vi.mock('../../../api/client', () => ({ ApiError: class extends Error { status = 0 }, useRadarFeature: () => ({ gatedKey: [], guard: (request: () => unknown) => request() }) }))
vi.mock('../../../api/config', () => ({ getApiBase: () => '/api' }))
vi.mock('../../../contexts/CapabilitiesContext', () => ({ useCapabilitiesContext: () => ({ deployment: { mode: 'local' } }) }))
vi.mock('../../../context/ConnectionContext', () => ({ useConnection: () => ({ connection: { context: 'test' } }) }))
vi.mock('../../../api/igdebug', () => ({
  fetchIGStatus: vi.fn(), fetchIGRun: (...args: unknown[]) => mock.fetch(...args),
  startIGCapture: (...args: unknown[]) => mock.start(...args),
  stopIGRun: (...args: unknown[]) => mock.stop(...args),
}))
vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)

const target = { namespace: 'app', pod: 'api', container: 'app', podUid: 'old', node: 'node-a', containerId: '123' }
const pod = (uid = 'old', containerID = '123') => ({
  metadata: { namespace: 'app', name: 'api', uid },
  spec: { containers: [{ name: 'app' }] },
  status: { containerStatuses: [{ name: 'app', containerID, state: { running: {} } }] },
})
const result = { id: 'run-1', target, state: 'complete', aspect: 'processes', result: {
  target, aspect: 'processes', processes: [{ command: 'old-process', pid: 1 }],
  scope: 'container', capturedAt: '2026-10-13T00:00:00Z', eventCount: 1, eventLoss: 'unmeasurable', truncated: false, captureDurationMs: 0,
} }
let element: HTMLDivElement
let root: Root
beforeEach(() => {
  mock.status = { data: { state: 'installed' }, isLoading: false }
  mock.start.mockReset(); mock.fetch.mockReset(); mock.stop.mockReset(); mock.stop.mockResolvedValue({}); mock.query.mockClear()
  element = document.createElement('div'); root = createRoot(element)
})
afterEach(async () => { await act(async () => root.unmount()) })
const expand = () => Array.from(element.querySelectorAll('button')).find(button => button.textContent?.includes('Live Debug'))!.click()
const capture = () => Array.from(element.querySelectorAll('button')).find(button => button.textContent?.includes('Capture'))!

describe('Live Debug capture identity', () => {
  it('blocks duplicate submissions while the create request is pending', async () => {
    let resolve!: (run: any) => void
    mock.start.mockReturnValue(new Promise(done => { resolve = done }))
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    await act(async () => expand())
    await act(async () => { capture().click(); capture().click() })
    expect(mock.start).toHaveBeenCalledTimes(1)
    expect(capture().disabled).toBe(true)
    await act(async () => resolve(result))
    expect(element.textContent).toContain('old-process')
    expect(element.textContent).toContain('Pod UID: old')
    expect(element.textContent).toContain('Container ID: 123')
  })

  it.each(['pod', 'container'])('ignores a capture response after the %s is replaced', async replacement => {
    let resolve!: (run: any) => void
    mock.start.mockReturnValue(new Promise(done => { resolve = done }))
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    await act(async () => expand())
    await act(async () => capture().click())
    await act(async () => root.render(<LiveDebugSection data={replacement === 'pod' ? pod('new') : pod('old', '456')} />))
    await act(async () => resolve(result))
    expect(element.textContent).not.toContain('old-process')
    expect(mock.stop).toHaveBeenCalledWith('run-1')
    expect(capture().disabled).toBe(false)
  })

  it.each(['pod', 'container'])('retains a partial capture and the chosen container after the %s restarts', async replacement => {
    let resolve!: (run: any) => void
    mock.start.mockResolvedValue({ id: 'run-1', target, aspect: 'processes', state: 'running' })
    mock.fetch.mockReturnValue(new Promise(done => { resolve = done }))
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    await act(async () => expand())
    await act(async () => capture().click())
    await act(async () => root.render(<LiveDebugSection data={replacement === 'pod' ? pod('new', '456') : pod('old', '456')} />))
    expect(mock.stop).not.toHaveBeenCalled()
    await act(async () => resolve({ ...result, state: 'partial', reason: 'target container was replaced' }))
    expect(element.textContent).toContain('target container was replaced')
    expect(element.textContent).toContain('These results refer to the displayed Pod UID and container ID')
    expect(element.textContent).toContain('old-process')
  })

  it('enables Capture when a container starts after the drawer opened', async () => {
    const waiting = pod()
    waiting.status.containerStatuses = []
    await act(async () => root.render(<LiveDebugSection data={waiting} />))
    await act(async () => expand())
    expect(capture().disabled).toBe(true)
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    expect(capture().disabled).toBe(false)
  })

  it('renders an upgrade note for an older backend, and hides an absent installation', async () => {
    mock.status = { error: new RadarFeatureUnsupportedError('liveDebug', { currentVersion: 'v1.16.0' }) }
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    expect(element.textContent).toContain('Live Debug captures need a newer Radar')
    expect(element.querySelector('button')?.getAttribute('aria-expanded')).toBe('false')
    expect(element.textContent).not.toContain('could not be verified')
    mock.status = { data: { state: 'not-installed' } }
    await act(async () => root.render(<LiveDebugSection data={pod()} />))
    expect(element.textContent).toBe('')
  })
})
