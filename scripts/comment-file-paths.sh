#!/usr/bin/env bash
# scripts/comment-file-paths.sh

show_help() {
    echo 'Usage: add_file_comments [OPTIONS] [FOLDER]'
    echo 'Add or update file path comments at the beginning of specified file types.'
    echo
    echo 'Options:'
    echo '  --help              Show this help message and exit'
    echo '  --file-types TYPES  Comma-separated list of file extensions to process (default: tsx,ts,sh,css,py,astro,go,yml,yaml)'
    echo '  --copy-contents     Copy contents of processed files into a single file (default: false)'
    echo
    echo 'If no FOLDER is provided, the script runs from the current working directory.'
}

parse_arguments() {
    file_types='tsx,ts,sh,css,py,astro,go,yml,yaml'
    copy_contents=false
    target_dir='.'

    while [[ $# -gt 0 ]]; do
        case $1 in
        --help | -h)
            show_help
            return 1
            ;;
        --file-types | -f)
            file_types="$2"
            shift 2
            ;;
        --copy-contents | -c)
            copy_contents=true
            shift
            ;;
        *)
            target_dir="$1"
            shift
            ;;
        esac
    done

    IFS=',' read -ra file_types <<< "$file_types"
    target_dir_abs=$(realpath "$target_dir")
    echo "Target directory: $target_dir_abs"
    echo "File types to process: ${file_types[*]}"
    echo "Copy contents: $copy_contents"
}

# Cross-platform function to get file permissions
get_file_permissions() {
    local file="$1"
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS (BSD stat)
        stat -f %A "$file"
    else
        # Linux (GNU stat)
        stat -c %a "$file"
    fi
}

add_file_comment() {
    local file="$1"
    local root_dir=$(find_package_json_dir "$target_dir_abs")
    local relative_path=${file#"$root_dir/"}
    local file_extension="${file##*.}"
    local temp_file=$(mktemp)
    local original_permissions=$(get_file_permissions "$file")

    if [[ "$file_extension" == "sh" ]]; then
        local new_comment="# $relative_path"
        local first_line=$(head -n 1 "$file")
        local second_line=$(sed -n '2p' "$file")

        if [[ "$first_line" =~ ^#!/ ]]; then
            if [[ "$second_line" =~ ^#[[:space:]]* ]]; then
                local existing_path=$(echo "$second_line" | sed 's/^#[[:space:]]*//')
                if [[ "$existing_path" != "$relative_path" ]]; then
                    echo "$first_line" > "$temp_file"
                    echo "$new_comment" >> "$temp_file"
                    tail -n +3 "$file" >> "$temp_file"
                    echo "Updated comment in $relative_path (preserving shebang, was: $existing_path)"
                else
                    rm "$temp_file"
                    return
                fi
            else
                echo "$first_line" > "$temp_file"
                echo "$new_comment" >> "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Added comment to $relative_path (preserving shebang)"
            fi
        elif [[ "$first_line" =~ ^#[[:space:]]* ]]; then
            local existing_path=$(echo "$first_line" | sed 's/^#[[:space:]]*//')
            if [[ "$existing_path" != "$relative_path" ]]; then
                echo "$new_comment" > "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Updated comment in $relative_path (was: $existing_path)"
            else
                rm "$temp_file"
                return
            fi
        else
            echo "#!/usr/bin/env bash" > "$temp_file"
            echo "$new_comment" >> "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added comment and shebang to $relative_path"
        fi
    elif [[ "$file_extension" == "css" ]]; then
        local new_comment="/* $relative_path */"
        local first_line=$(head -n 1 "$file")

        if [[ "$first_line" =~ ^/\*[[:space:]]* ]]; then
            local existing_path=$(echo "$first_line" | sed 's/^\/\*[[:space:]]*//' | sed 's/[[:space:]]*\*\///')
            if [[ "$existing_path" != "$relative_path" ]]; then
                echo "$new_comment" > "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Updated comment in $relative_path (was: $existing_path)"
            else
                rm "$temp_file"
                return
            fi
        else
            echo "$new_comment" > "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added comment to $relative_path"
        fi
    elif [[ "$file_extension" == "py" ]]; then
        local new_comment="# $relative_path"
        local first_line=$(head -n 1 "$file")
        local second_line=$(sed -n '2p' "$file")

        if [[ "$first_line" =~ ^#!/usr/bin/env[[:space:]]+python ]]; then
            if [[ "$second_line" =~ ^#[[:space:]]* ]]; then
                local existing_path=$(echo "$second_line" | sed 's/^#[[:space:]]*//')
                if [[ "$existing_path" != "$relative_path" ]]; then
                    echo "$first_line" > "$temp_file"
                    echo "$new_comment" >> "$temp_file"
                    tail -n +3 "$file" >> "$temp_file"
                    echo "Updated comment in $relative_path (preserving env/python line, was: $existing_path)"
                else
                    rm "$temp_file"
                    return
                fi
            else
                echo "$first_line" > "$temp_file"
                echo "$new_comment" >> "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Added comment to $relative_path (preserving env/python line)"
            fi
        elif [[ "$first_line" =~ ^#[[:space:]]* ]]; then
            local existing_path=$(echo "$first_line" | sed 's/^#[[:space:]]*//')
            if [[ "$existing_path" != "$relative_path" ]]; then
                echo "$new_comment" > "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Updated comment in $relative_path (was: $existing_path)"
            else
                rm "$temp_file"
                return
            fi
        else
            echo "$new_comment" > "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added comment to $relative_path"
        fi
    elif [[ "$file_extension" == "astro" ]]; then
        local new_comment="// $relative_path"
        local first_three_lines=$(head -n 3 "$file")

        if [[ $(echo "$first_three_lines" | head -n 1) == "---" ]]; then
            if [[ $(echo "$first_three_lines" | sed -n '2p') =~ ^//[[:space:]]* ]]; then
                local existing_path=$(echo "$first_three_lines" | sed -n '2p' | sed 's/^\/\/[[:space:]]*//')
                if [[ "$existing_path" != "$relative_path" ]]; then
                    echo "---" > "$temp_file"
                    echo "$new_comment" >> "$temp_file"
                    tail -n +3 "$file" >> "$temp_file"
                    echo "Updated comment in $relative_path (was: $existing_path)"
                else
                    rm "$temp_file"
                    return
                fi
            else
                echo "---" > "$temp_file"
                echo "$new_comment" >> "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Added comment to $relative_path"
            fi
        else
            echo "---" > "$temp_file"
            echo "$new_comment" >> "$temp_file"
            echo "---" >> "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added frontmatter and comment to $relative_path"
        fi

    elif [[ "$file" == *".d.ts" ]]; then
        local new_comment="// $relative_path"
        local temp_content=""
        local line
        local line_num=0
        local old_comment_line_num=-1
        local insert_after_line=0
        local correct_comment_exists=false

        # First pass: Check for existing correct comment and find insertion point/old comment
        while IFS= read -r line || [[ -n "$line" ]]; do
            ((line_num++))
            # Check for the correct comment
            if [[ "$line" =~ ^//[[:space:]]* && "$(echo "$line" | sed 's/^\/\/[[:space:]]*//')" == "$relative_path" ]]; then
                correct_comment_exists=true
                break # Found correct comment, no changes needed
            fi

            # Track the end of triple-slash directives
            if [[ "$line" =~ ^///[[:space:]]*\<reference ]]; then
                insert_after_line=$line_num
            elif [[ "$old_comment_line_num" -eq -1 && "$line" =~ ^//[[:space:]]* ]]; then
                # Found a potential old comment (only consider the first one after triple slashes)
                local potential_path=$(echo "$line" | sed 's/^\/\/[[:space:]]*//')
                # Heuristic: check if it looks like a path comment
                if [[ "$potential_path" == *"/"* || "$potential_path" == *.* ]]; then
                    old_comment_line_num=$line_num
                fi
                # Stop checking for old comments after the first non-triple-slash line
                if [[ ! "$line" =~ ^///[[:space:]]*\<reference ]]; then
                    break
                fi
            elif [[ ! "$line" =~ ^///[[:space:]]*\<reference ]]; then
                # Stop checking once past triple-slash directives and non-comment lines
                break
            fi
        done < "$file"

        if [[ "$correct_comment_exists" == true ]]; then
            rm "$temp_file"
            # echo "Correct comment already exists in $relative_path" # Optional: reduce noise
            return
        fi

        # Second pass: Build the new content
        line_num=0
        local added_comment=false
        while IFS= read -r line || [[ -n "$line" ]]; do
            ((line_num++))
            # Skip the old comment line if found
            if [[ "$old_comment_line_num" -ne -1 && "$line_num" -eq "$old_comment_line_num" ]]; then
                continue
            fi

            temp_content+="$line"$'\n'

            # Insert the new comment immediately after the triple-slash block (or at the beginning)
            if [[ "$added_comment" == false && "$line_num" -eq "$insert_after_line" ]]; then
                temp_content+="$new_comment"$'\n'
                added_comment=true
            fi
        done < "$file"

        # If the file was empty or only contained triple-slash directives
        if [[ "$added_comment" == false ]]; then
            temp_content="$new_comment"$'\n'"$temp_content"
        fi

        # Remove trailing newline if present
        temp_content="${temp_content%$'\n'}"

        echo "$temp_content" > "$temp_file"
        if [[ "$old_comment_line_num" -ne -1 ]]; then
            echo "Updated comment in $relative_path (removed old comment at line $old_comment_line_num)"
        else
            echo "Added comment to $relative_path"
        fi
    elif [[ "$file_extension" == "ts" || "$file_extension" == "tsx" || "$file_extension" == "go" ]]; then
        local new_comment="// $relative_path"
        local first_line=$(head -n 1 "$file")

        if [[ "$first_line" =~ ^//[[:space:]]* ]]; then
            local existing_path=$(echo "$first_line" | sed 's/^\/\/[[:space:]]*//')
            if [[ "$existing_path" != "$relative_path" ]]; then
                echo "$new_comment" > "$temp_file"
                tail -n +2 "$file" >> "$temp_file"
                echo "Updated comment in $relative_path (was: $existing_path)"
            else
                rm "$temp_file"
                return
            fi
        else
            echo "$new_comment" > "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added comment to $relative_path"
        fi
        
    elif [[ "$file_extension" == "yml" || "$file_extension" == "yaml" ]]; then
        local new_comment="# $relative_path"
        local first_line=$(head -n 1 "$file")

        if [[ "$first_line" =~ ^#[[:space:]]* ]]; then
            local existing_path=$(echo "$first_line" | sed 's/^#[[:space:]]*//')
            if [[ "$existing_path" != "$relative_path" ]]; then
                echo "$new_comment" > "$temp_file"
                tail -n 2 "$file" >> "$temp_file"
                echo "Updated comment in $relative_path (was: $existing_path)"
            else
                rm "$temp_file"
                return
            fi
        else
            echo "$new_comment" > "$temp_file"
            cat "$file" >> "$temp_file"
            echo "Added comment to $relative_path"
        fi

    fi

   

    cat "$temp_file" > "$file"
    rm "$temp_file"

    chmod "$original_permissions" "$file"
}

find_package_json_dir() {
    local current_dir="$1"
    while [[ "$current_dir" != "/" ]]; do
        if [[ -f "$current_dir/package.json" ]]; then
            echo "$current_dir"
            return 0
        fi
        current_dir=$(dirname "$current_dir")
    done
    echo "$1"
    return 1
}

get_gitignore_patterns() {
    local gitignore_file="$target_dir_abs/.gitignore"
    local patterns=()

    if [[ -f "$gitignore_file" ]]; then
        while IFS= read -r line; do
            # Skip empty lines and comments
            if [[ -n "$line" && ! "$line" =~ ^# ]]; then
                # Remove trailing slashes and add pattern
                line="${line%/}"
                patterns+=("-not -path \"*/$line/*\"")
            fi
        done < "$gitignore_file"
    fi

    # Always exclude some common directories
    patterns+=("-not -path \"*/venv/*\"")
    patterns+=("-not -path \"*/node_modules/*\"")
    patterns+=("-not -path \"*/.*\"")

    echo "${patterns[*]}"
}

process_files() {
    local files=()
    local exclude_patterns
    exclude_patterns=$(get_gitignore_patterns)

    for ext in "${file_types[@]}"; do
        while IFS= read -r -d '' file; do
            files+=("$file")
        done < <(eval "find \"$target_dir_abs\" -type f -name \"*.$ext\" $exclude_patterns -print0")
    done

    if [[ ${#files[@]} -eq 0 ]]; then
        echo "No matching files found in $target_dir_abs"
        return
    fi

    echo 'Processing files...'
    for file in "${files[@]}"; do
        add_file_comment "$file"
    done
    echo 'Finished processing files.'

    if [[ "$copy_contents" == true ]]; then
        echo 'Copying contents to combined file...'
        local combined_file='combined_contents.txt'
        > "$combined_file"
        for file in "${files[@]}"; do
            echo "// File: ${file#"$target_dir_abs/"}" >> "$combined_file"
            cat "$file" >> "$combined_file"
            echo -e "\n\n" >> "$combined_file"
        done
        echo "Contents copied to $combined_file"
    fi
}

if ! parse_arguments "$@"; then
    exit 1
fi

current_dir=$(pwd)
cd "$target_dir_abs" || exit 1

process_files

cd "$current_dir" || exit 1