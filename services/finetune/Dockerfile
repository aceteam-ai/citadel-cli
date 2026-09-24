FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

RUN python -m pip install --no-cache-dir \
    'torch==2.5.1' \
    xformers==0.0.28.post3 \
    transformers==4.51.3 \
    peft==0.15.2 \
    trl==0.15.2 \
    bitsandbytes==0.45.5 \
    unsloth==2025.4.7 \
    unsloth_zoo==2025.4.4 \
    datasets==3.5.0

COPY train.py /app/train.py
USER 65532:65532
ENTRYPOINT ["python", "/app/train.py"]
