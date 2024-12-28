#!/bin/bash

# Function to check if a command exists
check_command() {
    if ! command -v "$1" &> /dev/null; then
        echo "Error: $1 is not installed"
        echo "Please install $1 first:"
        case "$1" in
            "docker")
                echo "Visit: https://docs.docker.com/get-docker/"
                ;;
            "act")
                echo "Install using one of:"
                echo "  Ubuntu/Debian: curl -fsSL https://raw.githubusercontent.com/nektos/act/master/install.sh | sudo bash"
                echo "  macOS: brew install act"
                echo "  Windows: choco install act-cli"
                ;;
            "gh")
                echo "Install GitHub CLI from: https://cli.github.com/"
                ;;
        esac
        exit 1
    fi
}

# Check prerequisites
echo "Checking prerequisites..."
check_command "docker"
check_command "act"

# Check Docker is running
if ! docker info &> /dev/null; then
    echo "Error: Docker is not running"
    echo "Please start Docker and try again"
    exit 1
fi

# Check GitHub CLI authentication (optional)
GH_TOKEN=""
if command -v gh &> /dev/null && gh auth status &> /dev/null; then
    GH_TOKEN="$(gh auth token)"
fi

# Show usage
show_usage() {
    echo "Usage: $0 [options]"
    echo "Options:"
    echo "  -j, --job <job>    Run specific job (default: test-amd64)"
    echo "  -l, --list         List available jobs"
    echo "  -v, --verbose      Show verbose output"
    echo "  -h, --help         Show this help message"
    echo
    echo "Available jobs:"
    echo "  amd64-build        Build AMD64 binary"
    echo "  arm64-build        Build ARM64 binary"
    echo "  test-amd64         Run tests with AMD64 binary"
    exit 1
}

# Parse arguments
JOB="test-amd64"
VERBOSE=""

while [[ $# -gt 0 ]]; do
    case $1 in
        -j|--job)
            JOB="$2"
            shift 2
            ;;
        -l|--list)
            echo "Available jobs in workflow:"
            act -l
            exit 0
            ;;
        -v|--verbose)
            VERBOSE="--verbose"
            shift
            ;;
        -h|--help)
            show_usage
            ;;
        *)
            echo "Unknown option: $1"
            show_usage
            ;;
    esac
done

# Create artifacts directory
mkdir -p /tmp/artifacts

echo "Running GitHub Action locally..."
echo "Job: $JOB"
echo "This will execute the selected job using act..."

# Run the specified job using Ubuntu runner
act -j "$JOB" \
    -P ubuntu-latest=nektos/act-environments-ubuntu:18.04 \
    --bind \
    ${GH_TOKEN:+-s GITHUB_TOKEN="$GH_TOKEN"} \
    -s ACTIONS_RUNTIME_TOKEN="$(openssl rand -hex 16)" \
    --artifact-server-path /tmp/artifacts \
    $VERBOSE

status=$?
if [ $status -eq 0 ]; then
    echo "Workflow completed successfully!"
else
    echo "Workflow failed with exit code: $status"
    echo "Check the output above for errors"
fi

exit $status
