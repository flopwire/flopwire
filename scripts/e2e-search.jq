# One JSON page from ranked search on the empty synthetic E2E stack.
# Page's optional zero values use omitempty (internal/retrieval/format).
def absent_or($key; $value): (has($key) | not) or .[$key] == $value;

length == 1 and (.[0] |
  type == "object" and
  .hits == [] and
  (has("error") | not) and
  absent_or("total"; 0) and
  absent_or("total_sessions"; 0) and
  absent_or("offset"; 0) and
  absent_or("next_offset"; 0) and
  absent_or("truncated"; false) and
  absent_or("reason"; "") and
  absent_or("sessions"; []) and
  absent_or("session_info"; [])
)
