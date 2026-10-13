import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { RadarUpgradeNote, getDefaultContainerName, getDefaultRunningContainerName } from '@skyhook-io/k8s-ui'
import {
  LiveDebugSection as BaseLiveDebugSection,
  type LiveDebugAspect,
  type LiveDebugRun,
} from '@skyhook-io/k8s-ui/components/resources/renderers/LiveDebugSection'
import { ApiError, useRadarFeature } from '../../../api/client'
import { RadarFeatureUnsupportedError } from '../../../api/radarFeatures'
import { getApiBase } from '../../../api/config'
import { useCapabilitiesContext } from '../../../contexts/CapabilitiesContext'
import { useConnection } from '../../../context/ConnectionContext'
import { fetchIGRun, fetchIGStatus, startIGCapture, stopIGRun } from '../../../api/igdebug'
import { Section } from '@skyhook-io/k8s-ui/components/ui/drawer-components'
import { Activity } from 'lucide-react'

interface LiveDebugSectionProps {
  data: any
  onNavigate?: (ref: { kind: string; namespace: string; name: string }) => void
}

export function LiveDebugSection({ data, onNavigate }: LiveDebugSectionProps) {
  const apiBase = getApiBase()
  const cloud = useCapabilitiesContext().deployment?.mode === 'cloud'
  const feature = useRadarFeature('liveDebug')
  const { connection } = useConnection()
  const namespace = data.metadata?.namespace || ''
  const pod = data.metadata?.name || ''
  const containers = useMemo(() => (data.spec?.containers || []).map((container: any) => container.name as string), [data.spec?.containers])
  const defaultContainer = getDefaultRunningContainerName(data) || getDefaultContainerName(data) || ''
  const [selectedContainer, setSelectedContainer] = useState(defaultContainer)
  const [run, setRun] = useState<LiveDebugRun>()
  const [error, setError] = useState<string>()
  const activeRun = useRef<LiveDebugRun | undefined>(undefined)
  activeRun.current = run
  const currentConnection = useRef(connection.context)
  currentConnection.current = connection.context
  const [pending, setPending] = useState(false)
  const generation = useRef(0)
  const busy = useRef(false)
  const containerStatus = data.status?.containerStatuses?.find((status: any) => status.name === selectedContainer)
  const containerID = containerStatus?.containerID
  const currentContainerID = useRef(containerID)
  currentContainerID.current = containerID
  const running = containerStatus?.state?.running != null
  const normalizedContainerID = containerID?.replace(/^.*?:\/\//, '')
  const targetChanged = !!run && (run.target.podUid !== data.metadata?.uid || (!!run.target.containerId && !!normalizedContainerID && run.target.containerId !== normalizedContainerID))
  const targetKey = `${apiBase}:${connection.context}:${data.metadata?.uid}:${selectedContainer}`
  const viewKey = `${apiBase}:${connection.context}:${namespace}/${pod}:${selectedContainer}`
  const currentTarget = useRef(targetKey)
  currentTarget.current = targetKey
  const runID = run?.id
  const runState = run?.state
  const statusQuery = useQuery({
    queryKey: ['ig', 'status', apiBase, connection.context, ...feature.gatedKey],
    enabled: !cloud,
    queryFn: () => feature.guard(fetchIGStatus),
    staleTime: 15_000,
    retry: false,
  })

  useEffect(() => {
    if (!containers.includes(selectedContainer)) setSelectedContainer(defaultContainer)
  }, [containers, selectedContainer, defaultContainer])

  useEffect(() => {
    setSelectedContainer(defaultContainer)
    setRun(undefined)
    setError(undefined)
  }, [namespace, pod])

  useEffect(() => {
    generation.current++
    busy.current = false
    setPending(false)
    setRun(undefined)
    setError(undefined)
    return () => {
      generation.current++
      const current = activeRun.current
      if (current && !['complete', 'stopped', 'failed', 'partial'].includes(current.state)
          && getApiBase() === apiBase && currentConnection.current === connection.context) {
        void stopIGRun(current.id).catch(() => {})
      }
    }
  }, [viewKey])

  useEffect(() => {
    generation.current++
    busy.current = false
    setPending(false)
  }, [targetKey])

  useEffect(() => {
    if (!runID || !runState || ['complete', 'stopped', 'failed', 'partial'].includes(runState)) return
    let cancelled = false
    const target = targetKey
    const abort = new AbortController()
    let timer: number | undefined
    const poll = async () => {
      try {
        const next = await fetchIGRun(runID, abort.signal)
        if (!cancelled && currentTarget.current === target) setRun(next)
      } catch (pollError) {
        if (!cancelled && currentTarget.current === target) {
          const message = pollError instanceof Error ? pollError.message : String(pollError)
          setError(message)
          if (pollError instanceof ApiError && (pollError.status === 403 || pollError.status === 404)) {
            setRun(previous => previous ? { ...previous, state: 'failed', error: message } : previous)
            return
          }
        }
      }
      if (!cancelled) timer = window.setTimeout(poll, 1_000)
    }
    void poll()
    return () => {
      cancelled = true
      abort.abort()
      window.clearTimeout(timer)
    }
  }, [runID, runState, targetKey])

  if (cloud) return null

  if (statusQuery.error instanceof RadarFeatureUnsupportedError) {
    return <Section title="Live Debug" icon={Activity} defaultExpanded={false}><div className="mb-2 text-xs text-theme-text-secondary">Requires an existing Inspektor Gadget deployment and a direct Radar connection.</div><RadarUpgradeNote requirement={statusQuery.error.radarUpgrade} /></Section>
  }

  if (statusQuery.isLoading || statusQuery.data?.state === 'not-installed') return null

  const capture = async (aspect: LiveDebugAspect, durationSeconds: number) => {
    if (!running || busy.current || (run && !['complete', 'stopped', 'failed', 'partial'].includes(run.state))) return
    busy.current = true
    setPending(true)
    const token = ++generation.current
    const target = targetKey
    const runtimeID = containerID
    setError(undefined)
    setRun(undefined)
    try {
      const next = await startIGCapture({ namespace, pod, podUid: data.metadata.uid, containerId: containerID, container: selectedContainer, aspect, durationSeconds })
      if (generation.current === token && currentTarget.current === target && (runtimeID == null || currentContainerID.current === runtimeID)) setRun(next)
      else if (getApiBase() === apiBase && currentConnection.current === connection.context) {
        void stopIGRun(next.id).catch(() => {})
      }
    } catch (captureError) {
      if (generation.current === token && currentTarget.current === target) setError(captureError instanceof Error ? captureError.message : String(captureError))
    } finally {
      if (generation.current === token && currentTarget.current === target) {
        busy.current = false
        setPending(false)
      }
    }
  }

  const stop = async () => {
    if (!run) return
    const token = generation.current
    const target = targetKey
    try {
      const next = await stopIGRun(run.id)
      if (generation.current === token && currentTarget.current === target) setRun(next)
    } catch (stopError) {
      if (generation.current === token && currentTarget.current === target) setError(stopError instanceof Error ? stopError.message : String(stopError))
    }
  }

  const changeContainer = (container: string) => {
    setSelectedContainer(container)
    setRun(undefined)
    setError(undefined)
  }

  const status = statusQuery.data?.state === 'installed' ? 'installed' : 'unknown'
  return (
    <BaseLiveDebugSection
      status={status}
      pending={pending}
      targetChanged={targetChanged}
      captureUnavailableReason={!running ? 'Capture needs a running container. This container has no reported running state.' : undefined}
      statusMessage={statusQuery.data?.message || (statusQuery.error as Error | undefined)?.message}
      containers={containers}
      selectedContainer={selectedContainer}
      onContainerChange={changeContainer}
      run={run}
      error={error}
      onCapture={capture}
      onStop={stop}
      onNavigate={onNavigate}
    />
  )
}
