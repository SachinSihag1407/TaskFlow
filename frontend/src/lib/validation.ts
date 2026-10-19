/** Client-side mirror of the server's password policy for instant feedback. */
export function passwordProblem(pw: string): string | null {
  if (pw.length < 10) return "Use at least 10 characters.";
  if (pw.length > 128) return "Use at most 128 characters.";
  if (!/\p{L}/u.test(pw) || !/[\p{N}\p{P}\p{S}\s]/u.test(pw))
    return "Include a letter and a number or symbol.";
  return null;
}
