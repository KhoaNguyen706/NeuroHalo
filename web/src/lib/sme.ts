// How many more trials an ERP line needs before its SME reaches a target.
//
// The SME of an average shrinks with the square root of the trials in it
// (SD / √n), so a line with n trials and SME s reaches target t at about
// n × (s / t)² trials. Trials still to be rejected are allowed for: if a
// quarter of the trials so far were left out, a quarter of the new ones will
// be too. It is an estimate; the real SME moves as the trials come in.

// trialsStillNeeded returns how many more trials (markers to present) the
// line needs: 0 when its SME is at or under the target, null when it cannot
// tell (fewer than 2 trials kept, or no SME).
// Used by: the session page's erpQuality.
export function trialsStillNeeded(kept: number, recorded: number, sme: number, target: number): number | null {
  if (!(kept >= 2) || !(sme > 0) || !(target > 0) || recorded < kept) return null;
  if (sme <= target) return 0;
  const keptNeeded = Math.ceil(kept * (sme / target) ** 2) - kept;
  return Math.ceil(keptNeeded * (recorded / kept));
}
