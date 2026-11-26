-- Pure predicate for an effective kind derived from the ORIGINAL stream entry.
-- No Redis calls, cjson value coercion, decoded maps or execution projection.
-- Return true only for private MEDIA_GENERATE retention, never ACK authority.
return function(kind, raw)
  if kind ~= 'MEDIA_GENERATE' or raw == '' then return false end
  local length = #raw
  local function space(i)
    return string.find(raw, '[^ \t\r\n]', i) or length + 1
  end
  local function digit(b)
    return b and b >= 48 and b <= 57
  end
  local function hex(b)
    if b >= 48 and b <= 57 then return b - 48 end
    if b >= 65 and b <= 70 then return b - 55 end
    if b >= 97 and b <= 102 then return b - 87 end
    return nil
  end
  local function string_end(i)
    i = i + 1
    while i <= length do
      -- Skip ordinary prompt bytes in the string library without copying them.
      local j = string.find(raw, '["\\%z\1-\31]', i)
      if not j then return nil end
      local b = string.byte(raw, j)
      if b == 34 then return j + 1 end
      if b < 32 then return nil end
      local escaped = string.byte(raw, j + 1)
      if escaped == 117 then
        if j + 5 > length then return nil end
        for n = j + 2, j + 5 do
          if not hex(string.byte(raw, n)) then return nil end
        end
        i = j + 6
      elseif escaped == 34 or escaped == 92 or escaped == 47 or
             escaped == 98 or escaped == 102 or escaped == 110 or
             escaped == 114 or escaped == 116 then
        i = j + 2
      else
        return nil
      end
    end
    return nil
  end
  local function inputs_key(start, finish)
    -- Six ASCII characters need at most six Unicode escapes plus quotes.
    if finish - start > 38 then return false end
    local wanted, count, i = 'inputs', 0, start + 1
    while i < finish - 1 do
      local b = string.byte(raw, i)
      if b == 92 then
        if string.byte(raw, i + 1) ~= 117 then return false end
        b = 0
        for n = i + 2, i + 5 do b = b * 16 + hex(string.byte(raw, n)) end
        i = i + 6
      else
        i = i + 1
      end
      count = count + 1
      if count > 6 or b ~= string.byte(wanted, count) then return false end
    end
    return count == 6
  end
  local function number_end(i)
    local b = string.byte(raw, i)
    if b == 45 then i = i + 1; b = string.byte(raw, i) end
    if b == 48 then
      i = i + 1
    elseif b and b >= 49 and b <= 57 then
      i = string.find(raw, '[^0-9]', i) or length + 1
    else
      return nil
    end
    if string.byte(raw, i) == 46 then
      i = i + 1
      if not digit(string.byte(raw, i)) then return nil end
      i = string.find(raw, '[^0-9]', i) or length + 1
    end
    b = string.byte(raw, i)
    if b == 69 or b == 101 then
      i = i + 1
      b = string.byte(raw, i)
      if b == 43 or b == 45 then i = i + 1 end
      if not digit(string.byte(raw, i)) then return nil end
      i = string.find(raw, '[^0-9]', i) or length + 1
    end
    return i
  end

  local i = space(1)
  if string.byte(raw, i) ~= 123 then return true end
  -- Iterative JSON grammar: no recursive call-stack dependence. States:
  -- 1 object first-key/end, 2 key after comma, 3 colon, 4 object value,
  -- 5 object comma/end, 6 array first-value/end, 7 value after comma,
  -- 8 array comma/end. Only depth-1 object keys are privacy inputs.
  local states, depth = {1}, 1
  i = i + 1
  while depth > 0 do
    i = space(i)
    if i > length then return true end
    local state, b = states[depth], string.byte(raw, i)
    if state == 1 or state == 2 then
      if b == 125 and state == 1 then
        depth = depth - 1
        i = i + 1
      elseif b == 34 then
        local finish = string_end(i)
        if not finish then return true end
        if depth == 1 and inputs_key(i, finish) then return true end
        states[depth], i = 3, finish
      else
        return true
      end
    elseif state == 3 then
      if b ~= 58 then return true end
      states[depth], i = 4, i + 1
    elseif state == 5 or state == 8 then
      local closing = state == 5 and 125 or 93
      if b == closing then
        depth = depth - 1
        i = i + 1
      elseif b == 44 then
        states[depth] = state == 5 and 2 or 7
        i = i + 1
      else
        return true
      end
    elseif state == 6 and b == 93 then
      depth = depth - 1
      i = i + 1
    else
      states[depth] = state == 4 and 5 or 8
      if b == 123 or b == 91 then
        depth = depth + 1
        -- This matches encoding/json's container nesting limit.
        if depth > 10000 then return true end
        states[depth] = b == 123 and 1 or 6
        i = i + 1
      elseif b == 34 then
        i = string_end(i)
        if not i then return true end
      elseif b == 116 and string.sub(raw, i, i + 3) == 'true' then
        i = i + 4
      elseif b == 102 and string.sub(raw, i, i + 4) == 'false' then
        i = i + 5
      elseif b == 110 and string.sub(raw, i, i + 3) == 'null' then
        i = i + 4
      else
        i = number_end(i)
        if not i then return true end
      end
    end
  end
  return space(i) <= length
end
