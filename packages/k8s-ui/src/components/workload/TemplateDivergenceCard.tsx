import { GitCompare } from 'lucide-react'
import type { ResourceRef, TemplateDivergence } from '../../types'
import { ResourceRefBadge } from '../ui/drawer-components'

// Shows how a Pod (or a workload's Pods) differs from the template that
// created it. The server only sends the field when something differs, so an
// older Radar, which never sends it, renders nothing here.
export function TemplateDivergenceCard({
  divergence,
  namespace,
  onNavigate,
}: {
  divergence?: TemplateDivergence
  namespace: string
  onNavigate?: (ref: ResourceRef) => void
}) {
  if (!divergence || !divergence.differences?.length) return null
  const workload = Boolean(divergence.examplePod)
  const sources = divergence.sourceRefs ?? []
  const gaps = divergence.sourceGaps ?? []

  return (
    <section className="rounded-lg border border-theme-border bg-theme-surface p-4 shadow-theme-sm" aria-label="Live spec differs from template">
      <div className="mb-3 flex min-w-0 items-center gap-2">
        <GitCompare className="h-4 w-4 shrink-0 text-theme-text-secondary" aria-hidden />
        <h2 className="truncate text-[11px] font-semibold uppercase tracking-wide text-theme-text-secondary">Live spec differs from template</h2>
      </div>
      <div className="flex flex-col gap-3 text-xs">
        <p className="text-theme-text-secondary">
          {workload ? (
            <>
              {divergence.podsDiffering} of {divergence.podsCompared} {divergence.podsCapped ? 'most recent ' : ''}Pods don&apos;t match their template
              {divergence.podsNotCompared ? `, and ${divergence.podsNotCompared} could not be compared` : ''}.
            </>
          ) : (
            <>This Pod&apos;s running spec doesn&apos;t match its template{divergence.baseline ? ` in ${divergence.baseline}` : ''}.</>
          )}
        </p>
        {workload && divergence.examplePod && (
          <div className="flex flex-wrap items-center gap-1.5 text-theme-text-tertiary">
            <span>Differences for</span>
            <ResourceRefBadge resourceRef={{ kind: 'Pod', namespace, name: divergence.examplePod }} onClick={onNavigate} wrapAtSeparator />
            {divergence.baseline && <span>against {divergence.baseline}</span>}
          </div>
        )}
        <ul className="flex flex-col gap-1 rounded-md border border-theme-border/70 px-2.5 py-2">
          {divergence.differences.map((difference) => (
            <li key={difference} className="break-words font-mono text-[11px] leading-relaxed text-theme-text-secondary">{difference}</li>
          ))}
        </ul>
        {sources.length > 0 && (
          <div className="flex flex-col gap-1.5">
            <span className="text-[11px] font-semibold uppercase tracking-[0.06em] text-theme-text-tertiary">Could have changed it</span>
            <div className="flex flex-wrap gap-1.5">
              {sources.map((ref) => (
                <ResourceRefBadge
                  key={`${ref.group ?? ''}/${ref.kind}/${ref.namespace ?? ''}/${ref.name}`}
                  resourceRef={{ kind: ref.kind, group: ref.group, namespace: ref.namespace ?? '', name: ref.name }}
                  onClick={onNavigate}
                  wrapAtSeparator
                />
              ))}
              {divergence.sourcesNotNamed ? <span className="self-center text-theme-text-tertiary">+{divergence.sourcesNotNamed} more</span> : null}
            </div>
          </div>
        )}
        {gaps.map((gap) => (
          <p key={gap} className="text-theme-text-tertiary">{gap}</p>
        ))}
        <p className="text-theme-text-tertiary">
          Admission webhooks, LimitRange defaults, an in-place resize or a direct edit can each cause this. Radar can&apos;t see which one did, and matching a Pod doesn&apos;t mean a source changed it.
        </p>
      </div>
    </section>
  )
}
