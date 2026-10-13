import type { HealthLevel } from '../resources/resource-utils'
import type { WorkspaceProblem } from '../problems'
import type { CNPGProblem, CNPGProblemCategory } from './workspace'

export const CNPG_PROBLEM_TONE: Record<CNPGProblem['severity'], HealthLevel> = {
  critical: 'unhealthy',
  warning: 'degraded',
  posture: 'neutral',
}

export function cnpgProblemForPresentation(problem: CNPGProblem): WorkspaceProblem<CNPGProblemCategory> {
  return { ...problem, tone: CNPG_PROBLEM_TONE[problem.severity] }
}
