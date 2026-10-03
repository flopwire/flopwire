export function nextPage(response: { next_cursor: string }) {
  return { cursor: response.next_cursor };
}
