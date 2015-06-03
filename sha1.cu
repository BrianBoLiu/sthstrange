#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <ctype.h>
#include <time.h>
#include <math.h>
#include <signal.h>

#include "sha1.h"

#define THREAD_LOOPS 100000

int sigintfired = 0;
void handle_sigint(int signo) {
    if (signo == SIGINT)
        sigintfired = 1;
}

double current_millis() {
	struct timespec spec;
	clock_gettime(CLOCK_REALTIME, &spec);
	
	double sec = spec.tv_sec;
	double ms = round(spec.tv_nsec / 1.0e6);
	return ms + 1000.0 * sec;
}

// Updates 20-byte SHA-1 record in 'hash' for 'num_blocks' consequtive 64-byte blocks
extern "C"
void sha1_update_intel(int *hash, unsigned char* input, size_t num_blocks );

typedef struct {
	char success;
	char nonce[17];
} result_t;

__device__ int compare(hash_digest_t *hash, unsigned char *target) {
	uint32_t *tmp = (uint32_t *)hash;
	#pragma unroll 5
	for (unsigned i = 0; i < 5; i++) {
		tmp[i] = swap(tmp[i]);
	}
	unsigned char *h = (unsigned char *)hash;
	#pragma unroll 20
	for (int i = 0; i < 20; i++) {
		if (h[i] == target[i]) continue;
		if (h[i] < target[i]) return 1;
		return 0;
	}
	return 0;
}

// Get as many threads as we like to simultaneously SHA1 data, store in results.
// data should already be padded etc.
__global__ void parallel_hash(const unsigned char *lastblock, const unsigned char *target, result_t *results, hash_digest_t hashinit) {
	// Copy the last block into local memory.
	unsigned char blk[64];
	memcpy(blk, lastblock, 64);
	
	// Copy the target into local memory
	unsigned char trg[20];
	memcpy(trg, target, 20);
	
	// The nonce
	unsigned char *nonce = &blk[64-25];
	
	// Take the first two chars to uniquely identify ourselves
	nonce[0] = '0' + blockIdx.x / 64;
	nonce[1] = '0' + blockIdx.x % 64;
	nonce[2] = '0' + threadIdx.x / 64;
	nonce[3] = '0' + threadIdx.x % 64;

	uint32_t w[16];
	hash_digest_t h;
	char success = 0;
	for (int i = 0; i < THREAD_LOOPS; i++) {
		int pos = i;
		// 64^4 is approx 16e6 so we should be good
		nonce[4] = '0' + (pos&0x3f); pos >>= 6;
		nonce[5] = '0' + (pos&0x3f); pos >>= 6;
		nonce[6] = '0' + (pos&0x3f); pos >>= 6;
		nonce[7] = '0' + (pos&0x3f); pos >>= 6;
		h = hashinit;
		computeSHA1Block(blk, w, &h);
		if (compare(&h, trg)) {
			success = 1;
			break;
		}
	}
	
	int idx = blockDim.x * blockIdx.x + threadIdx.x;
	results[idx].success = success;
	memcpy(results[idx].nonce, nonce, 16);
	results[idx].nonce[16] = '\0';
}

int get_difficulty(char *hex, unsigned char *target) {
	if (strlen(hex) > 40)
		return 0;
	for (int i = 0; i < strlen(hex); i++) {
		char c = hex[i];
		if (!isxdigit(c))
			return 0;
		int num = ('0' <= c && c <= '9') ? c - '0' : ('a' <= c && c <= 'f') ? c - 'a' + 10 : c - 'A' + 10;
		if (i % 2 == 0)
			target[i/2] |= num << 4;
		else
			target[i/2] |= num;
	}
	return 1;
}

int main(int argc, char *argv[]) {
	// Must be used with a difficulty or "bench"
	if (argc != 2) {
		printf("Usage: %s <difficulty in hex | bench>\n", argv[0]);
		return 1;
	}
	
	// Check for benchmark flag
	int bench = 0;
	if (strcmp(argv[1], "bench") == 0)
		bench = 1;
	
	// Read difficulty
	unsigned char target[20] = {0};
	if (!bench) {
		// In benchmarking mode, all zeros.
		if (!get_difficulty(argv[1], target)) {
			printf("Difficulty must be a hex string no longer than 40 chars.\n");
			return 1;
		}
	}
	
	// DEBUG difficulty
	// for (int i = 0; i < 20; i++) printf("%d ", target[i]); putchar('\n');
	
	// Read data
	unsigned char data[1024] = {0};
	int n, nread = 0;
	
	if (bench) {
		// In benchmarking mode, prefill the data.
		nread = 295;
		memset(data, 'A', nread);
	} else {
		while ((n = fread(data, 1, sizeof(data) - nread, stdin)) != 0)
			nread += n;
	}
	
	if (nread % 64 != 39) {
		printf("nread mod 64 = %d, wanted 39\n", nread % 64);
		return 1;
	}
	
	// Place the nonce
	unsigned char *nonce = &data[nread];
	memset(nonce, '0', 16);
	nread += 16;
	
	// Finish the SHA1 padding
	int nbits = 8*nread;
	int pos = nread;
	data[pos++] = 0x80;
	pos += 8;
	data[pos-1] = nbits & 0xff;
	data[pos-2] = (nbits & 0xff00) >> 8;
	data[pos-3] = (nbits & 0xff0000) >> 16;
	
	// DEBUG data
	// for (int i = 0; i < pos; i++) printf("%d ", data[i]); putchar('\n');
	
	// Hash most of it
	hash_digest_t hashinit = SHA1_DIGEST_INIT;
	if (pos/64 - 1 > 0)
		sha1_update_intel((int*)&hashinit, data, pos/64 - 1);
	
	int nblocks = 120;
	int nthreads = 64;
	int nprocs = nblocks * nthreads;
	
	dim3 dimGrid(nblocks);
	dim3 dimBlock(nthreads);
	
	// Allocate some device memory for threads to store their results.
	result_t *results_dev;
	cudaMalloc(&results_dev, nprocs * sizeof(result_t));
	result_t *results = (result_t *) malloc(sizeof(result_t) * nprocs);
	
	// Pass the last data block to the device.
	unsigned char *data_dev;
	cudaMalloc(&data_dev, sizeof(data));
	unsigned char *lastblock_dev = &data_dev[pos-64];

	// Pass the target to the device.
	unsigned char *target_dev;
	cudaMalloc(&target_dev, sizeof(target));
	cudaMemcpy(target_dev, target, sizeof(target), cudaMemcpyHostToDevice);

	int runs = 1;
	result_t *answer = NULL;
    signal(SIGINT, handle_sigint);
	double start = current_millis();
	for (;answer == NULL;runs++) {
        // Check for early exit
        if (sigintfired) {
            printf("SIGINT Receieved, exiting.\n");
            return 1;
        }
		// Modify the nonce
		int pos = runs;
		// 64^4 is approx 16e6 so we should be good
		nonce[10] = '0' + (pos&0x3f); pos >>= 6;
		nonce[11] = '0' + (pos&0x3f); pos >>= 6;
		nonce[12] = '0' + (pos&0x3f); pos >>= 6;
		nonce[13] = '0' + (pos&0x3f); pos >>= 6;
		nonce[14] = '0' + (pos&0x3f); pos >>= 6;
		nonce[15] = '0' + (pos&0x3f); pos >>= 6;

        /*
        nonce[16] = '\0';
        printf("%s\n", nonce);
        nonce[16] = 0x80;
        */

		// Copy the new data to the GPU
		cudaMemcpy(data_dev, data, sizeof(data), cudaMemcpyHostToDevice);

		// Launch the kernel.
		parallel_hash<<<dimGrid, dimBlock>>>(lastblock_dev, target_dev, results_dev, hashinit);

		cudaDeviceSynchronize();
		cudaError_t error = cudaGetLastError();
		if(error!=cudaSuccess) {
			fprintf(stderr,"ERROR: %s\n", cudaGetErrorString(error) );
			exit(-1);
		}
	
		// Copy the hashes back to main memory.
		cudaMemcpy(results, results_dev, sizeof(result_t) * nprocs, cudaMemcpyDeviceToHost);
		
		// Did we find anything? (Virtually no time impact)
		for (int i = 0; i < nblocks * nthreads; i++) {
			if (results[i].success) {
				answer = &results[i];
				break;
			}
		}
		
		// If we're benchmarking, quit after 10 runs.
		if (bench && runs == 10)
			break;
	}
	
	if (bench) {
		printf("Completed %d runs\n", runs);
		double end = current_millis();
		double secs = (end - start) / 1000.0;
		double hashes = (double)nprocs * (double)runs * (double)THREAD_LOOPS;
		printf("%.2f mhash/sec\n", hashes / secs / 1e6);
		return 0;
	}
	
	memcpy(nonce, answer->nonce, 16);
	fwrite(data, 1, nread, stdout);
	return 0;
}
	
	
	
	
